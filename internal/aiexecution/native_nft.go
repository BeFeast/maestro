package aiexecution

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
)

// NativeNftRules is provisioning text only. The launcher never installs rules;
// it observes the complete live ruleset against the reviewed installed digest.
func NativeNftRules(gatewayURL, forgejoIP string) (string, error) {
	u, err := url.Parse(gatewayURL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return "", Held("containment_gateway_invalid")
	}
	ip := net.ParseIP(u.Hostname())
	forge := net.ParseIP(forgejoIP)
	port, err := strconv.Atoi(u.Port())
	if ip == nil || ip.To4() == nil || forge == nil || forge.To4() == nil || err != nil || port <= 0 || port > 65535 {
		return "", Held("containment_gateway_invalid")
	}
	return fmt.Sprintf(`table inet maestro_native {
  chain output {
    type filter hook output priority 0; policy drop;
    ip daddr 127.0.0.0/8 accept
    ip daddr %s tcp dport %d accept
    ip daddr %s tcp dport 443 accept
  }
  chain input {
    type filter hook input priority 0; policy drop;
    ip saddr 127.0.0.0/8 ip daddr 127.0.0.0/8 accept
    ct state established,related accept
  }
  chain forward {
    type filter hook forward priority 0; policy drop;
  }
}
`, ip.String(), port, forge.String()), nil
}
