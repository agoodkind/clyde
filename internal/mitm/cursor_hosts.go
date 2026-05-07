package mitm

import (
	"net"
	"strings"
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
	switch host {
	case "api2.cursor.sh", "api3.cursor.sh":
		return true
	}
	return strings.HasSuffix(host, ".cursor.sh") ||
		strings.HasSuffix(host, ".cursor.com")
}
