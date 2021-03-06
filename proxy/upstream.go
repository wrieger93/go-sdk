package proxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/blend/go-sdk/logger"
)

// NewUpstream returns a new upstram.
func NewUpstream(target *url.URL) *Upstream {
	return &Upstream{
		URL:          target,
		ReverseProxy: httputil.NewSingleHostReverseProxy(target),
	}
}

// Upstream represents a proxyable server.
type Upstream struct {
	// Name is the name of the upstream.
	Name string
	// Log is a logger agent.
	Log *logger.Logger
	// URL represents the target of the upstream.
	URL *url.URL
	// ReverseProxy is what actually forwards requests.
	ReverseProxy *httputil.ReverseProxy
}

// WithName sets the name field of the upstream.
func (u *Upstream) WithName(name string) *Upstream {
	u.Name = name
	return u
}

// WithLogger sets the logger agent for the upstream.
func (u *Upstream) WithLogger(log *logger.Logger) *Upstream {
	u.Log = log
	return u
}

// ServeHTTP
func (u *Upstream) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if u.Log != nil {
		u.Log.Trigger(logger.NewWebRequestStartEvent(req))
	}
	start := time.Now()

	w := NewResponseWriter(rw)
	u.ReverseProxy.ServeHTTP(w, req)

	if u.Log != nil {
		wre := logger.NewWebRequestEvent(req).
			WithStatusCode(w.StatusCode()).
			WithContentLength(int64(w.ContentLength())).
			WithElapsed(time.Since(start))

		if value := w.Header().Get("Content-Type"); len(value) > 0 {
			wre = wre.WithContentType(value)
		}
		if value := w.Header().Get("Content-Encoding"); len(value) > 0 {
			wre = wre.WithContentEncoding(value)
		}

		u.Log.Trigger(wre)
	}
}
