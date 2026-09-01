package jsplugin

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ValidateRequestURL prevents plugins from directing a channel credential to
// hosts other than the configured base URL or an administrator-approved host.
func ValidateRequestURL(requestURL, baseURL string, allowedHosts []string) error {
	request, err := url.Parse(requestURL)
	if err != nil || request.Host == "" || request.Scheme != "http" && request.Scheme != "https" {
		return fmt.Errorf("plugin request URL must be absolute HTTP(S)")
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Host == "" || base.Scheme != "http" && base.Scheme != "https" {
		return fmt.Errorf("channel base URL is invalid")
	}
	if base.Scheme == "https" && request.Scheme == "http" {
		return fmt.Errorf("plugin request must not downgrade HTTPS to HTTP")
	}
	requestHost := canonicalHost(request)
	if requestHost == canonicalHost(base) {
		return nil
	}
	for _, allowed := range allowedHosts {
		allowedURL, parseErr := url.Parse("https://" + strings.TrimSpace(allowed))
		if parseErr == nil && requestHost == canonicalHost(allowedURL) {
			return nil
		}
	}
	return fmt.Errorf("plugin request host %q is not allowed", request.Host)
}

// ValidateCredentialedRedirectURL ensures an HTTP redirect cannot move plugin
// credentials to another origin. Redirect targets are also checked against the
// same administrator-approved host policy as direct plugin requests.
func ValidateCredentialedRedirectURL(requestURL, initialURL, baseURL string, allowedHosts []string) error {
	if err := ValidateRequestURL(requestURL, baseURL, allowedHosts); err != nil {
		return err
	}
	request, err := url.Parse(requestURL)
	if err != nil {
		return fmt.Errorf("plugin redirect URL is invalid")
	}
	initial, err := url.Parse(initialURL)
	if err != nil || initial.Host == "" || initial.Scheme != "http" && initial.Scheme != "https" {
		return fmt.Errorf("plugin initial request URL is invalid")
	}
	if initial.Scheme == "https" && request.Scheme == "http" {
		return fmt.Errorf("plugin redirect must not downgrade HTTPS to HTTP")
	}
	if initial.Scheme != request.Scheme || canonicalHost(initial) != canonicalHost(request) {
		return fmt.Errorf("credentialed plugin request cannot follow a cross-origin redirect")
	}
	return nil
}

func canonicalHost(value *url.URL) string {
	host := strings.ToLower(value.Hostname())
	port := value.Port()
	if port == "" || port == "80" && value.Scheme == "http" || port == "443" && value.Scheme == "https" {
		return host
	}
	return net.JoinHostPort(host, port)
}
