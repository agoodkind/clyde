package mitm

import (
	"net"
	"strings"
)

type cursorServiceHost string

const (
	cursorAPI2Host cursorServiceHost = "api2.cursor.sh"
	cursorAPI3Host cursorServiceHost = "api3.cursor.sh"
)

func shouldInterceptCursorConnect(target string) (string, bool) {
	host := strings.TrimSpace(target)
	if parsedHost, _, err := net.SplitHostPort(target); err == nil {
		host = parsedHost
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	return host, isCursorServiceHost(host)
}

func isCursorServiceHost(host string) bool {
	host = strings.Trim(strings.ToLower(host), ".")
	switch cursorServiceHost(host) {
	case cursorAPI2Host, cursorAPI3Host:
		return true
	}
	return strings.HasSuffix(host, ".cursor.sh") ||
		strings.HasSuffix(host, ".cursor.com")
}
