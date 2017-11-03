package proxy

import (
	"github.com/coyove/goflyway/pkg/logg"
	"github.com/coyove/goflyway/pkg/lru"

	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	_RETRY_OPPORTUNITIES = 2
)

type ServerConfig struct {
	Throttling     int64
	ThrottlingMax  int64
	UDPRelayListen int
	ProxyPassAddr  string
	DummyDomain    string

	Users map[string]UserConfig

	*Cipher
}

// for multi-users server, not implemented yet
type UserConfig struct {
	Auth          string
	Throttling    int64
	ThrottlingMax int64
}

type ProxyUpstream struct {
	tp            *http.Transport
	rp            http.Handler
	blacklist     *lru.Cache
	trustedTokens map[string]bool
	rkeyHeader    string

	*ServerConfig
}

func (proxy *ProxyUpstream) auth(auth string) bool {
	if _, existed := proxy.Users[auth]; existed {
		// we don't have multi-user mode currently
		return true
	}

	return false
}

func (proxy *ProxyUpstream) getIOConfig(auth string) *IOConfig {
	var ioc *IOConfig
	if proxy.Throttling > 0 {
		ioc = &IOConfig{Bucket: NewTokenBucket(proxy.Throttling, proxy.ThrottlingMax)}
	}

	return ioc
}

func (proxy *ProxyUpstream) Write(w http.ResponseWriter, r *http.Request, p []byte, code int) (n int, err error) {
	_, key, _ := proxy.Cipher.ReverseIV(r.Header.Get(proxy.rkeyHeader))

	if ctr := proxy.Cipher.getCipherStream(key); ctr != nil {
		ctr.XorBuffer(p)
	}

	w.WriteHeader(code)
	return w.Write(p)
}

func (proxy *ProxyUpstream) hijack(w http.ResponseWriter) net.Conn {
	hij, ok := w.(http.Hijacker)
	if !ok {
		logg.E("webserver doesn't support hijacking")
		return nil
	}

	conn, _, err := hij.Hijack()
	if err != nil {
		logg.E("hijacking: ", err.Error())
		return nil
	}

	return conn
}

func (proxy *ProxyUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// remote dns lookup
	if r.Method == "GET" && r.ProtoMajor == 1 && r.ProtoMinor == 0 && len(r.RequestURI) > 0 {
		cob := proxy.Cipher.DecryptString(r.RequestURI[1:])
		idx := strings.LastIndex(cob, "|")
		if idx == -1 {
			return
		}

		auth, host := cob[:idx], cob[idx+1:]

		if proxy.Users != nil && !proxy.auth(auth) {
			return
		}

		conn := proxy.hijack(w)
		if conn == nil {
			return
		}

		defer conn.Close()

		ip, err := net.ResolveIPAddr("ip4", host)
		if err != nil {
			// we assume the upstream can always query a valid ip, if not
			// we return 127.0.0.1 so the client will try directly connecting the host
			conn.Write([]byte{127, 0, 0, 1})
			return
		}

		conn.Write(ip.IP)
		return
	}

	replyRandom := func() {
		if proxy.rp == nil {
			round := proxy.Rand.Intn(32) + 32
			buf := make([]byte, 2048)
			for r := 0; r < round; r++ {
				ln := proxy.Rand.Intn(1024) + 1024

				for i := 0; i < ln; i++ {
					buf[i] = byte(proxy.Rand.Intn(256))
				}

				w.Write(buf[:ln])
				time.Sleep(time.Duration(proxy.Rand.Intn(100)) * time.Millisecond)
			}
		} else {
			proxy.rp.ServeHTTP(w, r)
		}
	}

	addr, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		logg.W("unknown address: ", r.RemoteAddr)
		replyRandom()
		return
	}

	rkey := r.Header.Get(proxy.rkeyHeader)
	options, rkeybuf, authbuf := proxy.Cipher.ReverseIV(rkey)

	if rkeybuf == nil {
		logg.W("can't find header, check your client's key, from: ", addr)
		proxy.blacklist.Add(addr, nil)
		replyRandom()
		return
	}

	var auth string
	if proxy.Users != nil {
		if authbuf == nil || string(authbuf) == "" || !proxy.auth(string(authbuf)) {
			logg.W("user auth failed, from: ", addr)
			return
		}

		auth = string(authbuf)
	}

	if options == 0 {
		r := isTrustedToken("unlock", rkeybuf)

		if r == -1 {
			logg.W("someone is using an old token: ", addr)
			proxy.blacklist.Add(addr, nil)
			replyRandom()
			return
		}

		if r == 1 {
			proxy.blacklist.Remove(addr)
			logg.L("unlock request accepted from: ", addr)
			return
		}
	}

	if h, _ := proxy.blacklist.GetHits(addr); h > _RETRY_OPPORTUNITIES {
		logg.W("repeated access using invalid key, from: ", addr)
		replyRandom()
		return
	}

	if (options&DO_CONNECT) > 0 || (options&DO_SOCKS5) > 0 {
		host := proxy.Cipher.DecryptDecompress(stripURI(r.RequestURI), rkeybuf...)
		if host == "" {
			replyRandom()
			return
		}

		logg.D("CONNECT ", options, " ", host)

		// dig tunnel
		downstreamConn := proxy.hijack(w)
		if downstreamConn == nil {
			return
		}

		ioc := proxy.getIOConfig(auth)

		// we are outside GFW and should pass data to the real target
		targetSiteConn, err := net.Dial("tcp", host)
		if err != nil {
			logg.E(err)
			return
		}

		if (options & DO_OMIT_HDR) == 0 {
			if (options & DO_CONNECT) > 0 {
				downstreamConn.Write(OK_HTTP)
			} else {
				downstreamConn.Write(OK_SOCKS)
			}
		}

		proxy.Cipher.Bridge(targetSiteConn, downstreamConn, rkeybuf, ioc)
	} else if (options & DO_FORWARD) > 0 {
		if !proxy.decryptRequest(r, options, rkeybuf) {
			replyRandom()
			return
		}

		logg.D(r.Method, " ", r.URL.String())

		r.Header.Del(proxy.rkeyHeader)
		resp, err := proxy.tp.RoundTrip(r)
		if err != nil {
			logg.E("proxy pass: ", r.URL, ", ", err)
			proxy.Write(w, r, []byte(err.Error()), http.StatusInternalServerError)
			return
		}

		if resp.StatusCode >= 400 {
			logg.D("[", resp.Status, "] - ", r.URL)
		}

		copyHeaders(w.Header(), resp.Header, proxy.Cipher, true, rkeybuf)
		w.WriteHeader(resp.StatusCode)

		iocc := proxy.Cipher.WrapIO(w, resp.Body, rkeybuf, proxy.getIOConfig(auth))
		iocc.Partial = false // HTTP must be fully encrypted

		nr, err := iocc.DoCopy()
		tryClose(resp.Body)

		if err != nil {
			logg.E("io.wrap ", nr, "bytes: ", err)
		}
	} else {
		proxy.blacklist.Add(addr, nil)
		replyRandom()
	}
}

func StartServer(addr string, config *ServerConfig) {
	word := genWord(config.Cipher, false)
	proxy := &ProxyUpstream{
		tp: &http.Transport{
			TLSClientConfig: tlsSkip,
		},

		ServerConfig:  config,
		blacklist:     lru.NewCache(128),
		trustedTokens: make(map[string]bool),
		rkeyHeader:    "X-" + word,
	}

	if config.ProxyPassAddr != "" {
		if strings.HasPrefix(config.ProxyPassAddr, "http") {
			u, err := url.Parse(config.ProxyPassAddr)
			if err != nil {
				logg.F(err)
			}

			logg.L("alternatively act as reverse proxy: ", config.ProxyPassAddr)
			proxy.rp = httputil.NewSingleHostReverseProxy(u)
		} else {
			logg.L("alternatively act as file server: ", config.ProxyPassAddr)
			proxy.rp = http.FileServer(http.Dir(config.ProxyPassAddr))
		}
	}

	if proxy.UDPRelayListen != 0 {
		l, err := net.ListenTCP("tcp", &net.TCPAddr{
			IP:   net.IPv6zero,
			Port: proxy.UDPRelayListen,
		})

		if err != nil {
			logg.F(err)
		}

		go func() {
			for {
				c, _ := l.Accept()
				go proxy.handleTCPtoUDP(c)
			}
		}()
	}

	if port, lerr := strconv.Atoi(addr); lerr == nil {
		addr = (&net.TCPAddr{IP: net.IPv4zero, Port: port}).String()
	}

	logg.L("Hi! ", word, ", server is listening at ", addr)
	logg.F(http.ListenAndServe(addr, proxy))
}
