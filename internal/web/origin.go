package web

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

const originKey = "web.requestOrigin"

func requestOrigin(c *gin.Context) string { return c.GetString(originKey) }

func sessionCookieName(c *gin.Context) string {
	if strings.HasPrefix(requestOrigin(c), "https://") {
		return "__Host-kimi_session"
	}
	return "kimi_session"
}

// The deployment's proxies preserve Host and overwrite X-Forwarded-Proto.
func externalOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if values := r.Header.Values("X-Forwarded-Proto"); len(values) > 0 {
		if len(values) != 1 || (values[0] != "https" && values[0] != "http") {
			return ""
		}
		scheme = values[0]
	}

	host, localHost := canonicalAuthority(r.Host, scheme)
	remote, _ := netip.ParseAddrPort(r.RemoteAddr)
	if host == "" || (scheme == "http" && (!localHost || !remote.Addr().Unmap().IsLoopback())) {
		return ""
	}
	return scheme + "://" + host
}

var dnsHost = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.?$`)

// Accept a hostname or IP literal with an optional port. Round-trip the authority
// before using it in an absolute OIDC redirect URI or an Origin comparison.
func canonicalAuthority(raw, scheme string) (authority string, loopback bool) {
	raw = strings.ToLower(raw)
	u := url.URL{Host: raw}
	host, port := u.Hostname(), u.Port()
	addr, err := netip.ParseAddr(host)
	name := host
	switch {
	case err == nil && addr.Is6() && addr.Zone() == "":
		name = "[" + host + "]"
	case err == nil && addr.Is4():
	case len(host) <= 254 && dnsHost.MatchString(host):
	default:
		return "", false
	}
	expected := name
	if port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return "", false
		}
		expected = net.JoinHostPort(host, port)
		port = strconv.FormatUint(n, 10)
	}
	if expected != raw {
		return "", false
	}
	if addr.IsValid() && addr.Is6() {
		name = "[" + addr.String() + "]"
	}
	loopback = host == "localhost" || addr.Unmap().IsLoopback()
	if port == "" || (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		return name, loopback
	}
	return name + ":" + port, loopback
}
