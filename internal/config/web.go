package config

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

var bcryptHash = regexp.MustCompile(`^\$2[aby]\$[0-9]{2}\$[./A-Za-z0-9]{53}$`)

type WebConfig struct {
	Enabled      bool       `yaml:"enabled"`
	Listen       string     `yaml:"listen"`
	Username     string     `yaml:"username"`
	PasswordHash string     `yaml:"password_hash"`
	OIDC         OIDCConfig `yaml:"oidc"`
}

type OIDCConfig struct {
	Issuer       string `yaml:"issuer"`
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
}

// Validate checks the effective web settings before any listener is opened.
func (w WebConfig) Validate() error {
	_, port, err := net.SplitHostPort(w.Listen)
	portNum, portErr := strconv.ParseUint(port, 10, 16)
	if err != nil || portErr != nil || portNum == 0 {
		return errors.New("web.listen 必须是带端口的监听地址，例如 :8080 或 [::1]:8080")
	}
	local := w.Username != "" || w.PasswordHash != ""
	if local {
		cost, err := bcrypt.Cost([]byte(w.PasswordHash))
		if strings.TrimSpace(w.Username) == "" || w.Username != strings.TrimSpace(w.Username) || !bcryptHash.MatchString(w.PasswordHash) || err != nil || cost < bcrypt.DefaultCost {
			return errors.New("本地登录需要 web.username 和有效的 bcrypt 密码哈希（cost 至少为 10）")
		}
	}
	o := w.OIDC
	if o != (OIDCConfig{}) {
		issuer, err := url.Parse(o.Issuer)
		if err != nil || issuer.Scheme != "https" || issuer.Hostname() == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.ForceQuery || issuer.Fragment != "" {
			return errors.New("web.oidc.issuer 必须是有效的 HTTPS 颁发者地址")
		}
		if strings.TrimSpace(o.ClientID) == "" || strings.TrimSpace(o.ClientSecret) == "" {
			return errors.New("OIDC 登录需要 client_id 和 client_secret")
		}
	} else if !local {
		return errors.New("启用 Web 控制台需要配置本地登录或 OIDC 登录")
	}
	return nil
}
