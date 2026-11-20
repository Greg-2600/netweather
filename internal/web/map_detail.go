package web

import (
	"fmt"
	"strings"

	"github.com/Greg-2600/netweather/internal/board"
)

// tileEntity answers "what is this box?" in one sentence. The map shrinks tiles
// until most cannot carry a name, so the hover card has to supply the context
// that a bare ticker (or a colour-only cell) cannot.
func tileEntity(t board.Target) string {
	name := strings.TrimSpace(t.Name)
	host := strings.TrimSpace(t.Host)
	words := strings.Fields(name)

	if len(words) >= 3 && strings.EqualFold(words[0], "root") && strings.EqualFold(words[1], "server") {
		return "DNS root server — one of the authoritative root nameservers, queried for the com zone to test global DNS reachability."
	}

	probe := strings.ToUpper(strings.TrimSpace(t.Probe))
	if probe == "" {
		probe = "PROBE"
	}

	switch t.Class {
	case "country":
		tld := strings.ToLower(strings.TrimSpace(t.Country))
		country := strings.TrimSuffix(name, " ccTLD")
		if i := strings.Index(country, " ("); i > 0 {
			country = strings.TrimSpace(country[:i])
		}
		if tld != "" {
			return fmt.Sprintf("Country-code top-level domain .%s — authoritative DNS for %s.", tld, orDefault(country, "its namespace"))
		}
		return "Country-code top-level domain — authoritative DNS for its namespace."
	case "reference":
		if t.Probe == "doh" {
			return "Reference DNS-over-HTTPS resolver — a control endpoint used to compare answers from a second transport."
		}
		return "Reference anchor — a stable " + probe + " endpoint used as a control for the fleet."
	case "network":
		return "Network reachability anchor — " + probe + " probe of " + orDefault(host, "the configured host") + "."
	case "service":
		return "Public " + humanizeSector(t.Sector) + " service — " + probe + " probe of " + orDefault(host, "its public endpoint") + "."
	}
	return "Probe target — " + probe + " of " + orDefault(host, "the configured host") + "."
}

// tileWhy answers "why is it marked this colour?" in plain English. The state
// says what the classifier decided; the error class says what the probe saw.
// Hover cards show both so a red box is not a mystery.
func tileWhy(t board.Target) string {
	detail := tileSignal(t)
	switch t.State {
	case board.StateDown:
		return "Marked DOWN because " + detail + ". The failure is reproducible and attributed to the target, so it counts as an outage."
	case board.StateBlocked:
		return "Marked BLOCKED because " + detail + ". The host answers, but access is filtered or challenged; this is not an outage."
	case board.StateDegraded:
		return "Marked DEGRADED because " + detail + ". The target answers, but not well enough to be called healthy."
	case board.StateConfigError:
		return "Marked CONFIG ERROR because " + detail + ". The probe path or certificate setup at the target needs fixing."
	case board.StateUnknown:
		return "Marked NO DATA because " + detail + ". There is not enough evidence yet to call this target healthy or down."
	case board.StateUnmeasurable:
		return "Marked UNMEASURABLE because " + detail + ". The vantage cannot see the target, so no verdict is recorded."
	case board.StateUp, board.StateUpReachability:
		return "Marked UP because " + detail + ". The latest probes passed from the active vantages."
	default:
		return "State " + string(t.State) + ": " + detail + "."
	}
}

// tileSignal turns the raw classifier class into a clause that reads well after
// "because". Classes with spaces or parentheses are already human sentences.
func tileSignal(t board.Target) string {
	class := strings.TrimSpace(t.ErrorClass)
	if class == "" {
		if t.HTTPStatus != 0 {
			return fmt.Sprintf("the server returned HTTP %d", t.HTTPStatus)
		}
		return "no probe result has been recorded for this interval"
	}
	if strings.ContainsAny(class, " (") {
		return "the probe reported " + class
	}

	switch {
	case class == "dns_timeout":
		return "the DNS query timed out — the server did not answer within the probe window"
	case class == "dns_refused":
		return "the DNS server actively refused the query"
	case class == "dns_servfail":
		return "the DNS server returned SERVFAIL — it could not answer for the queried name"
	case class == "dns_nodata":
		return "the DNS server answered successfully but returned no address or delegation records"
	case class == "dns_nxdomain":
		return "the DNS server reported NXDOMAIN — the queried name does not exist"
	case class == "dns_error", class == "dns_format":
		return "the DNS response was malformed or unusable"
	case strings.HasPrefix(class, "dns_"):
		return "the DNS probe failed with " + class
	case class == "http_timeout":
		return "the HTTPS request timed out before a complete response arrived"
	case class == "http_reset":
		return "the connection was reset before the response arrived"
	case class == "http_request":
		return "the probe request was rejected — check the target's probe configuration"
	case class == "tls_handshake":
		return "the TLS handshake did not complete"
	case class == "tls_reset_after_hello":
		return "TLS was reset right after the client hello — consistent with filtering"
	case class == "tls_issuer_unexpected":
		return "the certificate issuer was outside the learned set"
	case strings.HasPrefix(class, "tls_"):
		return "the TLS check failed with " + class
	case class == "tcp_timeout":
		return "the TCP connection timed out"
	case strings.HasPrefix(class, "tcp_"):
		return "the TCP probe failed with " + class
	case class == "icmp_unreachable":
		return "the host reported the destination as unreachable"
	case class == "icmp_timeout":
		return "the ICMP echo timed out"
	case class == "icmp_loss_partial":
		return "some ICMP echoes were lost"
	case class == "doh_nxdomain":
		return "the DNS-over-HTTPS resolver reported that the name does not exist"
	case strings.HasPrefix(class, "challenge"):
		return "a bot-protection challenge page was returned"
	case class == "semantic_ok":
		return "the expected content was found in the response"
	case class == "reachability only", class == "reachability_only":
		return "the endpoint answered, but has no content to verify"
	case class == "no assessable content":
		return "the endpoint answered with a script shell rather than content"
	}
	return "the probe reported " + class
}

func tileStateLabel(st board.State) string {
	switch st {
	case board.StateUp, board.StateUpReachability:
		return "UP"
	case board.StateDown:
		return "DOWN"
	case board.StateBlocked:
		return "BLOCKED"
	case board.StateDegraded:
		return "DEGRADED"
	case board.StateConfigError:
		return "CONFIG ERROR"
	case board.StateUnmeasurable:
		return "UNMEASURABLE"
	default:
		return "NO DATA"
	}
}

// mapTip is the plain-text fallback for browsers that ignore the styled hover
// card (and the accessible label on the tile itself).
func mapTip(name, kind, state, detail, duration, why string) string {
	return fmt.Sprintf("%s\n%s\n%s · %s · %s\n%s\nClick to open the target drawer.",
		name, kind, state, detail, duration, why)
}

func humanizeSector(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "/", " / ")
	return orDefault(s, "web")
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
