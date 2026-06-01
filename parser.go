package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// ── Dispatcher ────────────────────────────────────────────────────────────────

func toSingBoxOutbound(configURL, protocol string) (string, string) {
	switch protocol {
	case "vmess":
		return parseVMess(configURL)
	case "vless":
		return parseVLess(configURL)
	case "trojan":
		return parseTrojan(configURL)
	case "ss":
		return parseShadowsocks(configURL)
	case "hy2":
		return parseHysteria2(configURL)
	case "hy":
		return parseHysteria(configURL)
	case "tuic":
		return parseTUIC(configURL)
	case "ssr":
		return parseSSR(configURL)
	}
	return "", "unsupported protocol: " + protocol
}

// ── VMess ─────────────────────────────────────────────────────────────────────

func parseVMessURItoD(data string) (map[string]interface{}, string) {
	u, err := url.Parse("vmess://" + data)
	if err != nil {
		return nil, "uri parse: " + err.Error()
	}
	uuid := u.User.Username()
	if uuid == "" {
		return nil, "missing uuid"
	}
	host := u.Hostname()
	if host == "" {
		return nil, "missing server"
	}
	portStr := u.Port()
	if portStr == "" {
		portStr = "443"
	}
	q := u.Query()
	sec := strings.ToLower(q.Get("security"))
	tlsVal := ""
	if sec == "tls" || sec == "xtls" {
		tlsVal = "tls"
	}
	d := map[string]interface{}{
		"id": uuid, "add": host, "port": portStr,
		"aid":         first(q.Get("aid"), q.Get("alterId"), "0"),
		"scy":         first(q.Get("encryption"), q.Get("scy"), "auto"),
		"net":         first(q.Get("type"), q.Get("net"), "tcp"),
		"tls":         tlsVal,
		"sni":         first(q.Get("sni"), q.Get("peer"), host),
		"path":        q.Get("path"),
		"host":        q.Get("host"),
		"serviceName": q.Get("serviceName"),
		"fp":          q.Get("fp"),
	}
	return d, ""
}

func parseVMess(raw string) (string, string) {
	data := strings.TrimPrefix(raw, "vmess://")
	if idx := strings.LastIndex(data, "#"); idx != -1 {
		data = data[:idx]
	}
	data = strings.TrimSpace(data)

	var d map[string]interface{}

	if strings.HasPrefix(data, "{") {
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			return "", "json: " + err.Error()
		}
	} else {
		var tryB64 []string
		tryB64 = append(tryB64, data)
		if lastAt := strings.LastIndex(data, "@"); lastAt > 0 {
			tryB64 = append(tryB64, data[:lastAt])
		}
		{
			clean := data
			for i, c := range data {
				if c != '+' && c != '/' && c != '=' &&
					c != '-' && c != '_' &&
					!(c >= 'A' && c <= 'Z') &&
					!(c >= 'a' && c <= 'z') &&
					!(c >= '0' && c <= '9') {
					clean = data[:i]
					break
				}
			}
			if clean != data && clean != "" {
				tryB64 = append(tryB64, clean)
			}
		}

		var parsed bool
		var b64Err error
		for _, candidate := range tryB64 {
			var decoded string
			decoded, b64Err = decodeBase64([]byte(candidate))
			if b64Err != nil {
				continue
			}
			var tmp map[string]interface{}
			if json.Unmarshal([]byte(decoded), &tmp) == nil {
				d = tmp
				parsed = true
				break
			}
		}
		if !parsed {
			atIdx := strings.Index(data, "@")
			qIdx := strings.Index(data, "?")
			if atIdx != -1 && (qIdx == -1 || atIdx < qIdx) {
				sanitized := sanitizeProxyURL("vmess://" + data)
				sanitized = strings.TrimPrefix(sanitized, "vmess://")
				var parseErr string
				d, parseErr = parseVMessURItoD(sanitized)
				if parseErr != "" {
					return "", parseErr
				}
			} else {
				if b64Err != nil {
					return "", "base64: " + b64Err.Error()
				}
				return "", "json: invalid vmess payload"
			}
		}
	}
	server := strings.TrimSpace(fmt.Sprintf("%v", d["add"]))
	if server == "" {
		return "", "missing server"
	}
	port, err := toPort(fmt.Sprintf("%v", d["port"]))
	if err != nil {
		return "", "port: " + err.Error()
	}
	uuid := strings.TrimSpace(fmt.Sprintf("%v", d["id"]))
	if uuid == "" {
		return "", "missing uuid"
	}
	alterId := 0
	if v, ok := d["aid"]; ok {
		switch x := v.(type) {
		case float64:
			alterId = int(x)
		case string:
			alterId, _ = strconv.Atoi(x)
		}
	}
	security := "auto"
	if s, _ := d["scy"].(string); s != "" {
		security = s
	}
	network := "tcp"
	if n, _ := d["net"].(string); n != "" {
		network = strings.ToLower(n)
	}
	switch network {
	case "xhttp", "splithttp", "kcp", "mkcp", "quic":
		return "", "unsupported transport: " + network
	}
	tls := ""
	if tlsVal, _ := d["tls"].(string); tlsVal == "tls" {
		sni := server
		if s, _ := d["sni"].(string); s != "" {
			sni = s
		} else if h, _ := d["host"].(string); h != "" {
			sni = h
		}
		tls = fmt.Sprintf(`,"tls":{"enabled":true,"insecure":true,"server_name":%q}`, sni)
	}
	return fmt.Sprintf(`{"type":"vmess","tag":"proxy","server":%q,"server_port":%d,"uuid":%q,"security":%q,"alter_id":%d%s%s}`,
		server, port, uuid, security, alterId, tls, vmessTransport(d, network)), ""
}

func vmessTransport(d map[string]interface{}, network string) string {
	path := strDefault(d["path"], "/")
	host := strDefault(d["host"], "")
	svcName := strDefault(d["serviceName"], strDefault(d["path"], ""))
	return buildTransportJSON(network, path, host, svcName)
}

// ── VLess ─────────────────────────────────────────────────────────────────────

var singboxSupportedFlows = map[string]bool{
	"":                        true,
	"xtls-rprx-vision":        true,
	"xtls-rprx-vision-udp443": true,
}

func parseVLess(raw string) (string, string) {
	u, err := url.Parse(sanitizeProxyURL(raw))
	if err != nil {
		return "", "url parse: " + err.Error()
	}
	uuid := normalizeUUID(u.User.Username())
	if uuid == "" {
		return "", "missing uuid"
	}
	server := u.Hostname()
	if server == "" {
		return "", "missing server"
	}
	port, err := toPort(u.Port())
	if err != nil {
		return "", "port: " + err.Error()
	}
	q := u.Query()
	security := strings.TrimSpace(strings.ToLower(q.Get("security")))
	network := strings.ToLower(q.Get("type"))
	if network == "" {
		network = "tcp"
	}
	switch network {
	case "xhttp", "splithttp", "kcp", "mkcp", "quic":
		return "", "unsupported transport: " + network
	}
	sni := first(q.Get("sni"), q.Get("peer"), server)
	flow := q.Get("flow")
	if !singboxSupportedFlows[flow] {
		flow = ""
	}
	tlsJSON, tlsErr := vlessTLS(security, sni, flow, q)
	if tlsErr != "" {
		return "", tlsErr
	}
	transport := buildTransportJSON(network, first(q.Get("path"), "/"), q.Get("host"),
		first(q.Get("serviceName"), q.Get("path")))
	return fmt.Sprintf(`{"type":"vless","tag":"proxy","server":%q,"server_port":%d,"uuid":%q%s%s}`,
		server, port, uuid, tlsJSON, transport), ""
}

func vlessTLS(security, sni, flow string, q url.Values) (string, string) {
	flowJSON := ""
	if flow != "" {
		flowJSON = fmt.Sprintf(`,"flow":%q`, flow)
	}
	switch security {
	case "tls", "xtls":
		s := fmt.Sprintf(`,"tls":{"enabled":true,"insecure":true,"server_name":%q`, sni)
		if fp := q.Get("fp"); fp != "" {
			s += fmt.Sprintf(`,"utls":{"enabled":true,"fingerprint":%q}`, fp)
		}
		if alpnStr := q.Get("alpn"); alpnStr != "" {
			ab, _ := json.Marshal(strings.Split(alpnStr, ","))
			s += fmt.Sprintf(`,"alpn":%s`, ab)
		}
		return flowJSON + s + "}", ""
	case "reality":
		pbk := q.Get("pbk")
		if pbk == "" {
			return "", "reality: missing public key (pbk)"
		}
		return flowJSON + fmt.Sprintf(`,"tls":{"enabled":true,"server_name":%q,"utls":{"enabled":true,"fingerprint":%q},"reality":{"enabled":true,"public_key":%q,"short_id":%q}}`,
			sni, first(q.Get("fp"), "chrome"), pbk, q.Get("sid")), ""
	case "none", "":
		return "", ""
	}
	return "", "unknown security: " + security
}

// ── Trojan ────────────────────────────────────────────────────────────────────

func parseTrojan(raw string) (string, string) {
	u, err := url.Parse(sanitizeProxyURL(raw))
	if err != nil {
		return "", "url parse: " + err.Error()
	}
	password := u.User.Username()
	if password == "" {
		return "", "missing password"
	}
	server := u.Hostname()
	if server == "" {
		return "", "missing server"
	}
	port, err := toPort(u.Port())
	if err != nil {
		return "", "port: " + err.Error()
	}
	q := u.Query()
	sni := first(q.Get("sni"), q.Get("peer"), server)
	tls := fmt.Sprintf(`,"tls":{"enabled":true,"insecure":true,"server_name":%q`, sni)
	if fp := q.Get("fp"); fp != "" {
		tls += fmt.Sprintf(`,"utls":{"enabled":true,"fingerprint":%q}`, fp)
	}
	tls += "}"
	network := strings.ToLower(q.Get("type"))
	switch network {
	case "xhttp", "splithttp", "kcp", "mkcp", "quic":
		return "", "unsupported transport: " + network
	}
	transport := buildTransportJSON(network, first(q.Get("path"), "/"), q.Get("host"),
		first(q.Get("serviceName"), q.Get("path")))
	return fmt.Sprintf(`{"type":"trojan","tag":"proxy","server":%q,"server_port":%d,"password":%q%s%s}`,
		server, port, password, tls, transport), ""
}

// ── Shadowsocks ───────────────────────────────────────────────────────────────

var singboxSupportedSSCiphers = map[string]bool{
	"aes-128-gcm": true, "aes-192-gcm": true, "aes-256-gcm": true,
	"aes-128-cfb": true, "aes-192-cfb": true, "aes-256-cfb": true,
	"aes-128-ctr": true, "aes-192-ctr": true, "aes-256-ctr": true,
	"chacha20-ietf-poly1305": true, "xchacha20-ietf-poly1305": true,
	"chacha20-ietf":                 true,
	"2022-blake3-aes-128-gcm":       true,
	"2022-blake3-aes-256-gcm":       true,
	"2022-blake3-chacha20-poly1305": true,
	"none": true, "plain": true,
}

func parseShadowsocks(raw string) (string, string) {
	trimmed := strings.TrimPrefix(raw, "ss://")
	if idx := strings.LastIndex(trimmed, "#"); idx != -1 {
		trimmed = trimmed[:idx]
	}
	trimmed = strings.TrimSpace(trimmed)

	var method, password, server string
	var port int

	fastPathOK := false
	if fastU, err := url.Parse("ss://" + trimmed); err == nil &&
		fastU.User != nil && fastU.Hostname() != "" {
		uname := fastU.User.Username()
		pwd, hasPwd := fastU.User.Password()
		host := fastU.Hostname()
		portStr := fastU.Port()
		if portStr == "" {
			portStr = "443"
		}
		var m, p string
		if hasPwd {
			m, p = uname, pwd
		} else {
			if d, derr := decodeBase64([]byte(uname)); derr == nil && strings.Contains(d, ":") {
				parts := strings.SplitN(d, ":", 2)
				m, p = parts[0], parts[1]
			}
		}
		if m != "" && host != "" {
			if pVal, perr := toPort(portStr); perr == nil {
				method, password, server, port = m, p, host, pVal
				fastPathOK = true
			}
		}
	}

	if !fastPathOK {
		atIdx := strings.LastIndex(trimmed, "@")
		if atIdx == -1 {
			b64Src := trimmed
			if qi := strings.Index(b64Src, "?"); qi != -1 {
				b64Src = b64Src[:qi]
			}
			decoded, err := decodeBase64([]byte(b64Src))
			if err != nil {
				decoded = trimmed
			}
			atIdx2 := strings.LastIndex(decoded, "@")
			if atIdx2 == -1 {
				return "", "missing @"
			}
			userPart := decoded[:atIdx2]
			hostPart := decoded[atIdx2+1:]
			if idx := strings.Index(hostPart, "?"); idx != -1 {
				hostPart = hostPart[:idx]
			}
			m, p, s, po, e := ssParseUserAndHost(userPart, hostPart)
			if e != "" {
				return "", e
			}
			method, password, server, port = m, p, s, po
		} else {
			userPart := trimmed[:atIdx]
			hostPart := trimmed[atIdx+1:]
			if idx := strings.Index(hostPart, "?"); idx != -1 {
				hostPart = hostPart[:idx]
			}
			m, p, s, po, e := ssParseUserAndHost(userPart, hostPart)
			if e != "" {
				return "", e
			}
			method, password, server, port = m, p, s, po
		}
	}

	method = strings.ToLower(method)
	if !singboxSupportedSSCiphers[method] {
		return "", fmt.Sprintf("unsupported cipher: %s", method)
	}
	if server == "" {
		return "", "missing server"
	}
	return fmt.Sprintf(`{"type":"shadowsocks","tag":"proxy","server":%q,"server_port":%d,"method":%q,"password":%q}`,
		server, port, method, password), ""
}

func ssParseUserAndHost(userPart, hostPart string) (method, password, server string, port int, errMsg string) {
	decodeUser := func(s string) string {
		if d, err := decodeBase64([]byte(s)); err == nil && strings.Contains(d, ":") {
			return d
		}
		if unescaped, err := url.PathUnescape(s); err == nil && unescaped != s {
			if d, err2 := decodeBase64([]byte(unescaped)); err2 == nil && strings.Contains(d, ":") {
				return d
			}
			if strings.Contains(unescaped, ":") {
				return unescaped
			}
		}
		if colonIdx := strings.Index(s, ":"); colonIdx != -1 {
			prefix := s[:colonIdx]
			suffix := s[colonIdx+1:]
			if d, err := decodeBase64([]byte(prefix)); err == nil && !strings.Contains(d, ":") {
				return d + ":" + suffix
			}
		}
		return s
	}

	decoded := decodeUser(userPart)
	parts := strings.SplitN(decoded, ":", 2)
	if len(parts) != 2 || parts[0] == "" {
		return "", "", "", 0, "invalid user info"
	}
	method = strings.TrimSpace(parts[0])
	password = parts[1]

	hostPart = strings.TrimSpace(hostPart)
	var portStr string
	if strings.HasPrefix(hostPart, "[") {
		closeBracket := strings.Index(hostPart, "]")
		if closeBracket == -1 {
			return "", "", "", 0, "invalid IPv6 host"
		}
		server = hostPart[1:closeBracket]
		rest := hostPart[closeBracket+1:]
		if strings.HasPrefix(rest, ":") {
			portStr = rest[1:]
		} else {
			portStr = "443"
		}
	} else {
		lastColon := strings.LastIndex(hostPart, ":")
		if lastColon == -1 {
			return "", "", "", 0, "missing port"
		}
		server = hostPart[:lastColon]
		portStr = hostPart[lastColon+1:]
	}

	if idx := strings.IndexFunc(portStr, func(r rune) bool { return r < '0' || r > '9' }); idx != -1 {
		portStr = portStr[:idx]
	}
	portStr = strings.TrimSpace(portStr)
	p, err := toPort(portStr)
	if err != nil {
		return "", "", "", 0, "port: " + err.Error()
	}
	return method, password, server, p, ""
}

// ── Hysteria2 ─────────────────────────────────────────────────────────────────

func parseHysteria2(raw string) (string, string) {
	trimmed := strings.TrimPrefix(raw, "hy2://")
	if i := strings.LastIndex(trimmed, "#"); i != -1 {
		trimmed = trimmed[:i]
	}
	queryStr := ""
	if i := strings.Index(trimmed, "?"); i != -1 {
		queryStr = trimmed[i+1:]
		trimmed = trimmed[:i]
	}
	lastAt := strings.LastIndex(trimmed, "@")
	if lastAt == -1 {
		return "", "missing @"
	}
	password := trimmed[:lastAt]
	hostPort := trimmed[lastAt+1:]
	if password == "" {
		return "", "missing password"
	}
	if i := strings.Index(hostPort, "/"); i != -1 {
		hostPort = hostPort[:i]
	}
	lastColon := strings.LastIndex(hostPort, ":")
	var server string
	var port int
	if lastColon == -1 {
		server = hostPort
		port = 443
	} else {
		portCandidate := hostPort[lastColon+1:]
		if _, perr := toPort(portCandidate); perr == nil {
			server = hostPort[:lastColon]
			port, _ = toPort(portCandidate)
		} else if strings.HasPrefix(hostPort, "[") {
			server = hostPort
			port = 443
		} else {
			return "", "missing port"
		}
	}
	if server == "" {
		return "", "missing server"
	}
	q, _ := url.ParseQuery(queryStr)
	obfsJSON := ""
	if obfs := q.Get("obfs"); obfs == "salamander" {
		if obfsPwd := q.Get("obfs-password"); obfsPwd != "" {
			obfsJSON = fmt.Sprintf(`,"obfs":{"type":"salamander","password":%q}`, obfsPwd)
		}
	}
	return fmt.Sprintf(`{"type":"hysteria2","tag":"proxy","server":%q,"server_port":%d,"password":%q%s,"tls":{"enabled":true,"insecure":true,"server_name":%q}}`,
		server, port, password, obfsJSON, first(q.Get("sni"), server)), ""
}

// ── Hysteria (v1) ─────────────────────────────────────────────────────────────

func parseHysteria(raw string) (string, string) {
	u, err := url.Parse(sanitizeProxyURL(raw))
	if err != nil {
		return "", "url parse: " + err.Error()
	}
	server := u.Hostname()
	if server == "" {
		return "", "missing server"
	}
	port, err := toPort(u.Port())
	if err != nil {
		return "", "port: " + err.Error()
	}
	q := u.Query()
	auth := first(q.Get("auth"), u.User.Username())
	if auth == "" {
		return "", "missing auth"
	}
	up, _ := strconv.Atoi(first(q.Get("upmbps"), "10"))
	down, _ := strconv.Atoi(first(q.Get("downmbps"), "50"))
	if up <= 0 {
		up = 10
	}
	if down <= 0 {
		down = 50
	}
	obfs := q.Get("obfs")
	obfsJSON := ""
	if obfs != "" {
		obfsJSON = fmt.Sprintf(`,"obfs":%q`, obfs)
	}
	return fmt.Sprintf(`{"type":"hysteria","tag":"proxy","server":%q,"server_port":%d,"up_mbps":%d,"down_mbps":%d,"auth_str":%q%s,"tls":{"enabled":true,"insecure":true,"server_name":%q}}`,
		server, port, up, down, auth, obfsJSON, first(q.Get("peer"), q.Get("sni"), server)), ""
}

// ── TUIC ──────────────────────────────────────────────────────────────────────

func parseTUIC(raw string) (string, string) {
	u, err := url.Parse(sanitizeProxyURL(raw))
	if err != nil {
		return "", "url parse: " + err.Error()
	}
	uuid := u.User.Username()
	if uuid == "" {
		return "", "missing uuid"
	}
	password, _ := u.User.Password()
	server := u.Hostname()
	if server == "" {
		return "", "missing server"
	}
	port, err := toPort(u.Port())
	if err != nil {
		return "", "port: " + err.Error()
	}
	q := u.Query()
	sni := first(q.Get("sni"), server)
	congestion := first(q.Get("congestion_control"), q.Get("congestion-controller"), "bbr")
	udpRelayMode := q.Get("udp_relay_mode")
	udpJSON := ""
	if udpRelayMode != "" {
		udpJSON = fmt.Sprintf(`,"udp_relay_mode":%q`, udpRelayMode)
	}
	return fmt.Sprintf(`{"type":"tuic","tag":"proxy","server":%q,"server_port":%d,"uuid":%q,"password":%q,"congestion_control":%q%s,"tls":{"enabled":true,"insecure":true,"server_name":%q}}`,
		server, port, uuid, password, congestion, udpJSON, sni), ""
}

// ── SSR ───────────────────────────────────────────────────────────────────────

func parseSSR(raw string) (string, string) {
	trimmed := strings.TrimPrefix(raw, "ssr://")
	if trimmed == "" {
		return "", "empty ssr url"
	}
	decoded, err := decodeBase64([]byte(trimmed))
	if err != nil {
		return "", "base64: " + err.Error()
	}
	params := ""
	if i := strings.Index(decoded, "/?"); i != -1 {
		params = decoded[i+2:]
		decoded = decoded[:i]
	} else if i := strings.Index(decoded, "?"); i != -1 {
		params = decoded[i+1:]
		decoded = decoded[:i]
	}
	parts := strings.SplitN(decoded, ":", 6)
	if len(parts) < 6 {
		return "", "invalid ssr format (need host:port:protocol:method:obfs:password)"
	}
	host, portStr, protocol, method, obfs, b64pass := parts[0], parts[1], parts[2], parts[3], parts[4], parts[5]
	_ = protocol
	_ = obfs
	passDecoded, err := decodeBase64([]byte(b64pass))
	if err != nil {
		return "", "base64 password: " + err.Error()
	}
	_, err = toPort(portStr)
	if err != nil {
		return "", "port: " + err.Error()
	}
	_ = host
	_ = method
	_ = params
	_ = passDecoded
	return "", "SSR not supported by sing-box (collect-only protocol)"
}

// ── Transport builder ─────────────────────────────────────────────────────────

func buildTransportJSON(network, path, host, grpcService string) string {
	if path == "" {
		path = "/"
	}
	switch network {
	case "ws":
		if host != "" {
			return fmt.Sprintf(`,"transport":{"type":"ws","path":%q,"headers":{"Host":%q}}`, path, host)
		}
		return fmt.Sprintf(`,"transport":{"type":"ws","path":%q}`, path)
	case "grpc":
		return fmt.Sprintf(`,"transport":{"type":"grpc","service_name":%q}`, grpcService)
	case "h2", "http":
		if host != "" {
			return fmt.Sprintf(`,"transport":{"type":"http","host":[%q],"path":%q}`, host, path)
		}
		return fmt.Sprintf(`,"transport":{"type":"http","path":%q}`, path)
	case "tcp":
		return ""
	case "httpupgrade":
		if host != "" {
			return fmt.Sprintf(`,"transport":{"type":"httpupgrade","path":%q,"host":%q}`, path, host)
		}
		return fmt.Sprintf(`,"transport":{"type":"httpupgrade","path":%q}`, path)
	case "splithttp", "xhttp":
		if host != "" {
			return fmt.Sprintf(`,"transport":{"type":"splithttp","path":%q,"host":%q}`, path, host)
		}
		return fmt.Sprintf(`,"transport":{"type":"splithttp","path":%q}`, path)
	}
	return ""
}

// ── URL helpers ───────────────────────────────────────────────────────────────

func sanitizeProxyURL(raw string) string {
	raw = strings.ReplaceAll(raw, "&amp;", "&")
	raw = strings.ReplaceAll(raw, "&lt;", "<")
	raw = strings.ReplaceAll(raw, "&gt;", ">")
	raw = strings.ReplaceAll(raw, "&quot;", `"`)
	raw = strings.ReplaceAll(raw, "&#39;", "'")

	raw = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, raw)

	schemeIdx := strings.Index(raw, "://")
	if schemeIdx == -1 {
		return raw
	}
	scheme := raw[:schemeIdx+3]
	rest := raw[schemeIdx+3:]

	const maxIter = 20
	for i := 0; i < maxIter; i++ {
		if !strings.Contains(rest, "%") {
			break
		}
		decoded, err := url.QueryUnescape(rest)
		if err != nil {
			decoded, err = url.PathUnescape(rest)
			if err != nil || decoded == rest {
				break
			}
		}
		if decoded == rest {
			break
		}
		if strings.ContainsAny(decoded, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x0b\x0c\x0e\x0f") {
			break
		}
		rest = decoded
	}

	frag := ""
	if fragIdx := strings.LastIndex(rest, "#"); fragIdx != -1 {
		frag = rest[fragIdx:]
		rest = rest[:fragIdx]
	}
	query := ""
	if queryIdx := strings.Index(rest, "?"); queryIdx != -1 {
		query = rest[queryIdx:]
		rest = rest[:queryIdx]
	}
	lastAt := strings.LastIndex(rest, "@")
	if lastAt == -1 {
		return scheme + rest + query + frag
	}
	return scheme + encodeUserInfo(rest[:lastAt]) + "@" + rest[lastAt+1:] + query + frag
}

func normalizeUUID(u string) string {
	if len(u) == 32 {
		allHex := true
		for _, c := range u {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				allHex = false
				break
			}
		}
		if allHex {
			return u[0:8] + "-" + u[8:12] + "-" + u[12:16] + "-" + u[16:20] + "-" + u[20:32]
		}
	}
	return u
}

func encodeUserInfo(s string) string {
	var buf strings.Builder
	for i := 0; i < len(s); i++ {
		b := s[i]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') ||
			b == '-' || b == '.' || b == '_' || b == '~' || b == '!' || b == '$' ||
			b == '&' || b == '\'' || b == '(' || b == ')' || b == '*' || b == '+' ||
			b == ',' || b == ';' || b == '=' || b == ':' {
			buf.WriteByte(b)
		} else {
			fmt.Fprintf(&buf, "%%%02X", b)
		}
	}
	return buf.String()
}

// ── coreIdentity ─────────────────────────────────────────────────────────────

func coreIdentity(line, protocol string) string {
	switch protocol {
	case "vmess":
		data := strings.TrimPrefix(line, "vmess://")
		if idx := strings.LastIndex(data, "#"); idx != -1 {
			data = data[:idx]
		}
		data = strings.TrimSpace(data)

		if !strings.HasPrefix(data, "{") {
			if decoded, err := decodeBase64([]byte(data)); err == nil {
				var d struct {
					Add  string      `json:"add"`
					Port interface{} `json:"port"`
					ID   string      `json:"id"`
				}
				if json.Unmarshal([]byte(decoded), &d) == nil && d.Add != "" {
					return fmt.Sprintf("vmess://%s:%v#%s", d.Add, d.Port, d.ID)
				}
			}
		}

		if strings.HasPrefix(data, "{") {
			var d struct {
				Add  string      `json:"add"`
				Port interface{} `json:"port"`
				ID   string      `json:"id"`
			}
			if json.Unmarshal([]byte(data), &d) == nil && d.Add != "" {
				return fmt.Sprintf("vmess://%s:%v#%s", d.Add, d.Port, d.ID)
			}
		}

		if atIdx := strings.Index(data, "@"); atIdx != -1 {
			u, err := url.Parse("vmess://" + data)
			if err == nil && u.Hostname() != "" {
				return fmt.Sprintf("vmess://%s:%s#%s", u.Hostname(), u.Port(), u.User.Username())
			}
		}

		return line
	case "ssr":
		data := strings.TrimPrefix(line, "ssr://")
		if idx := strings.LastIndex(data, "#"); idx != -1 {
			data = data[:idx]
		}
		decoded, err := decodeBase64([]byte(strings.TrimSpace(data)))
		if err != nil {
			return line
		}
		parts := strings.SplitN(decoded, ":", 6)
		if len(parts) < 2 {
			return line
		}
		return fmt.Sprintf("ssr://%s:%s", parts[0], parts[1])
	default:
		u, err := url.Parse(sanitizeProxyURL(line))
		if err != nil || u.Hostname() == "" {
			return line
		}
		return fmt.Sprintf("%s://%s@%s:%s", protocol, u.User.String(), u.Hostname(), u.Port())
	}
}

// ── renameTo ──────────────────────────────────────────────────────────────────

func renameTo(config, protocol, newName string) string {
	switch protocol {
	case "vmess":
		data := strings.TrimPrefix(config, "vmess://")
		fragIdx := strings.LastIndex(data, "#")
		if fragIdx != -1 {
			data = data[:fragIdx]
		}
		data = strings.TrimSpace(data)

		isURI := false
		if strings.HasPrefix(data, "{") {
			isURI = false
		} else {
			decoded, err := decodeBase64([]byte(data))
			if err == nil {
				var tmp map[string]interface{}
				if json.Unmarshal([]byte(decoded), &tmp) == nil {
					tmp["ps"] = newName
					keys := make([]string, 0, len(tmp))
					for k := range tmp {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					var buf bytes.Buffer
					buf.WriteByte('{')
					for i, k := range keys {
						if i > 0 {
							buf.WriteByte(',')
						}
						kj, _ := json.Marshal(k)
						vj, _ := json.Marshal(tmp[k])
						buf.Write(kj)
						buf.WriteByte(':')
						buf.Write(vj)
					}
					buf.WriteByte('}')
					return "vmess://" + base64.StdEncoding.EncodeToString(buf.Bytes())
				}
			}
			if atIdx := strings.Index(data, "@"); atIdx != -1 {
				isURI = true
			}
		}

		if !isURI {
			var d map[string]interface{}
			if err := json.Unmarshal([]byte(data), &d); err != nil {
				return config
			}
			d["ps"] = newName
			keys := make([]string, 0, len(d))
			for k := range d {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var buf bytes.Buffer
			buf.WriteByte('{')
			for i, k := range keys {
				if i > 0 {
					buf.WriteByte(',')
				}
				kj, _ := json.Marshal(k)
				vj, _ := json.Marshal(d[k])
				buf.Write(kj)
				buf.WriteByte(':')
				buf.Write(vj)
			}
			buf.WriteByte('}')
			return "vmess://" + base64.StdEncoding.EncodeToString(buf.Bytes())
		}

		return "vmess://" + data + "#" + url.PathEscape(newName)

	default:
		if idx := strings.Index(config, "#"); idx != -1 {
			return config[:idx] + "#" + url.PathEscape(newName)
		}
		return config + "#" + url.PathEscape(newName)
	}
}

// ── Utilities ─────────────────────────────────────────────────────────────────

func toPort(s string) (int, error) {
	s = strings.TrimSpace(s)
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n > 65535 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return n, nil
}

func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func strDefault(v interface{}, def string) string {
	if v == nil {
		return def
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return def
	}
	return s
}
