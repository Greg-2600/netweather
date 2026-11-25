#!/usr/bin/env python3
"""Generate the early field-trial target config for netweather.

Inputs
------
A saved IANA root zone file (the same source `netweather seed --country-anchors`
uses). Pass its path as the first argument, or set ROOT_ZONE. The root zone gives
us two real, operator-maintained inventories:

   * ccTLD NS delegations  -> ~399 country anchors (one DNS question per territory)
   * glue A records       -> ~100 network anchors (authoritative DNS server IPs)

Two smaller curated tables below add a second delegated NS for selected ccTLDs
and extra root-zone glue IPs. Both are verified against the same root zone at
generation time, so they remain operator-maintained inventory rather than
invented endpoints.

A small REACHABLE_NS table below overrides the lexicographically first NS for
zones whose oldest-listed nameserver no longer answers. Every override is still
a real NS record for that TLD in the root zone and is checked at generation
time, so the fleet keeps measuring genuine IANA delegations.

Service targets are a curated list of real, public, unauthenticated endpoints
across the sectors the design monitors. No endpoint here requires a login or
accepts terms per request (ACCEPTABLE_USE.md 5).

Output
------
config/fieldtrial-targets.yaml, a version-1 target file ready for
NETWEATHER_CONFIG. Every entry has a unique (host, port, probe, qname) identity,
which migration 0024 made storable.
"""

from __future__ import annotations

import collections
import json
import os
import pathlib
import re
import sys

ROOT_ZONE = sys.argv[1] if len(sys.argv) > 1 else os.environ.get("ROOT_ZONE")
OUT = sys.argv[2] if len(sys.argv) > 2 else "config/fieldtrial-targets.yaml"
if not ROOT_ZONE:
    sys.exit("usage: gen_fieldtrial_targets.py ROOT_ZONE [OUTPUT]")

# Human-readable ccTLD names (CLDR for two-letter codes, IANA root DB for IDNs).
# Shared with internal/seed/cctld_names.go so a regeneration keeps the same names.
REPO = pathlib.Path(__file__).resolve().parent.parent
NAMES = json.loads((REPO / "internal/seed/cctld_names.json").read_text(encoding="utf-8"))


def country_name(tld: str) -> str:
    return NAMES.get(tld.lower(), tld.upper()) + " ccTLD"

# ---------------------------------------------------------------- root zone ---

with open(ROOT_ZONE, encoding="utf-8", errors="replace") as handle:
    body = handle.read()
lines = []
for raw in body.split("\n"):
    line = raw.split(";", 1)[0]
    fields = line.split()
    if len(fields) >= 5 and fields[0].upper() not in ("$TTL", "$ORIGIN"):
        lines.append(fields)

# ccTLD -> lexicographically first NS (matches internal/seed.ParseCountryAnchors).
chosen_ns: dict[str, str] = {}
ns_by_tld: dict[str, set[str]] = collections.defaultdict(set)
for f in lines:
    name = f[0].lower().rstrip(".")
    if "." in name:
        continue
    if not (name.startswith("xn--") or (len(name) == 2 and name.isalpha() and name.isascii())):
        continue
    ns = ""
    for i in range(1, len(f) - 1):
        if f[i].upper() == "NS":
            ns = f[i + 1].lower().rstrip(".")
            break
    if not ns or "." not in ns:
        continue
    if not re.fullmatch(r"[a-z0-9-]+(\.[a-z0-9-]+)+", ns):
        continue
    ns_by_tld[name].add(ns)
    if name not in chosen_ns or ns < chosen_ns[name]:
        chosen_ns[name] = ns

# The lexicographically first NS is not always the reachable one. Every name
# below is an NS record for its TLD in this same root zone, selected after
# probing the whole delegated set from the field-trial vantage. The default
# choice would otherwise produce permanently DOWN anchors for zones whose
# oldest-listed nameserver has gone dark.
REACHABLE_NS: dict[str, str] = {
    "et": "c.nic.et",  # a.nic.et drops ~60% of UDP queries from this vantage
    "gu": "phloem.uoregon.edu",
    "iq": "ns01.trs-dns.net",
    "jo": "rip.psg.com",
    "kp": "ns2.kptc.kp",
    "mw": "mw-e.ns.nic.cz",
    "ne": "ns.intnet.ne",
    "np": "shikhar.mos.com.np",
    "sd": "pch.sis.sd",
    "uz": "ns2.uz",
    "xn--mgbpl2fh": "pch.sis.sd",  # Sudan (Arabic)
    "xn--mgbtx2b": "nsp-anycast.cmc.iq",  # Iraq (Arabic)
    "xn--mgbayh7gpa": "jo.cctld.authdns.ripe.net",  # Jordan (Arabic)
    "xn--ygbi2ammx": "ps.cctld.authdns.ripe.net",  # Palestine (Arabic)
}
for tld, ns in REACHABLE_NS.items():
    if ns not in ns_by_tld.get(tld, set()):
        sys.exit(f"REACHABLE_NS[{tld!r}] = {ns!r} is not delegated in the root zone")

# A second ccTLD observation asks a different delegated operator the same real
# question. Every pair below is present in this root zone and answered a SOA
# query for its zone from the field-trial egress. IDs are new so the original
# cctld-* targets keep their history.
SECONDARY_NS: list[tuple[str, str]] = [
    ("ac", "a2.nic.ac"),
    ("ai", "v0n1.nic.ai"),
    ("ar", "c.dns.ar"),
    ("ax", "ns3.alcom.fi"),
    ("be", "b.nsset.be"),
    ("bj", "ns-bj.nic.fr"),
    ("bs", "anyns.pch.net"),
    ("bz", "b0.cctld.afilias-nst.org"),
    ("cg", "sunic.sunet.se"),
    ("cm", "ns-cm.afrinic.net"),
    ("cv", "c.dns.pt"),
    ("de", "l.de.net"),
    ("dz", "ns1.nic.dz"),
    ("es", "c.nic.es"),
    ("gb", "ns0.ja.net"),
    ("gh", "ns1.nic.gh"),
    ("gp", "gp.cctld.authdns.ripe.net"),
    ("hn", "ns1.anycastdns.cz"),
    ("ie", "c.ns.ie"),
    ("jm", "ns.jm"),
    ("kh", "ns1.dns.net.kh"),
    ("kr", "c.dns.kr"),
    ("lb", "i.ns.lb"),
    ("ls", "ns-ls.afrinic.net"),
    ("ma", "dns.inria.fr"),
    ("mt", "b.ns.mt"),
    ("my", "e.nic.my"),
    ("pa", "ns.dns.br"),
    ("pk", "root-c2.pknic.pk"),
    ("ps", "fork.sth.dnsnode.net"),
    ("re", "f.ext.nic.fr"),
    ("sa", "c2.dns.sa"),
    ("sg", "ns4.apnic.net"),
    ("sl", "ns2.neoip.com"),
    ("ss", "pch.nic.ss"),
    ("sy", "pch.anycast.tld.sy"),
    ("tg", "ns1.nic.tg"),
    ("tv", "b.nic.tv"),
    ("uk", "dns2.nic.uk"),
    ("vu", "b.tldns.vu"),
    ("xn--1qqw23a", "tb.ngtld.cn"),
    ("xn--45brj9c", "ns2.registry.in"),
    ("xn--6qq986b3xl", "c.zdnscloud.com"),
    ("xn--80aswg", "anycast23.irondns.net"),
    ("xn--9dbq2a", "ac2.nstld.com"),
    ("xn--c1avg", "a2.nic.xn--c1avg"),
    ("xn--d1alf", "dns-mk.univie.ac.at"),
    ("xn--fjq720a", "v0n1.nic.xn--fjq720a"),
    ("xn--g2xx48c", "b.nic.xn--g2xx48c"),
    ("xn--io0a7i", "b.ngtld.cn"),
    ("xn--jvr189m", "v0n1.nic.xn--jvr189m"),
    ("xn--l1acc", "ns11.dns.mn"),
    ("xn--mgba7c0bbn0a", "b.nic.xn--mgba7c0bbn0a"),
    ("xn--mgbcpq6gpa1a", "ns01.trs-dns.net"),
    ("xn--mgbt3dhd", "dns2.emdns.uk"),
    ("xn--mxtq1m", "b.nic.xn--mxtq1m"),
    ("xn--nqv7f", "a2.nic.xn--nqv7f"),
    ("xn--rhqv96g", "ns2.teleinfoo.com"),
    ("xn--vermgensberatung-pwb", "b.nic.xn--vermgensberatung-pwb"),
    ("xn--wgbh1c", "ns2.dotmasr.eg"),
]
seen_secondary_ns: set[str] = set()
for tld, ns in SECONDARY_NS:
    if ns not in ns_by_tld.get(tld, set()):
        sys.exit(f"SECONDARY_NS[{tld!r}] = {ns!r} is not delegated in the root zone")
    if ns == REACHABLE_NS.get(tld, chosen_ns[tld]):
        sys.exit(f"SECONDARY_NS[{tld!r}] repeats the primary nameserver {ns!r}")
    if ns in seen_secondary_ns:
        sys.exit(f"SECONDARY_NS repeats nameserver {ns!r}")
    seen_secondary_ns.add(ns)

# Glue A records -> (IP, owner TLD, NS host). Used for network anchors.
glue: list[tuple[str, str, str]] = []
for f in lines:
    if len(f) >= 5 and f[3].upper() == "A":
        owner = f[0].lower().rstrip(".")
        ip = f[4]
        if re.fullmatch(r"\d{1,3}(\.\d{1,3}){3}", ip):
            parts = owner.split(".")
            tld = parts[-1] if parts else ""
            if tld and re.fullmatch(r"[a-z0-9-]+", tld):
                glue.append((ip, tld, owner))

# Extra network anchors use distinct root-zone glue A records not consumed by
# the first 100. Each IP answered a SOA query for the zone it is glue for from
# the field-trial egress; the ids are new so net-* history is untouched.
EXTRA_GLUE: list[tuple[str, str, str]] = [
    ("199.115.153.1", "ai", "v0n1.nic.ai"),
    ("192.5.5.241", "arpa", "f.ns.arpa"),
    ("217.14.111.13", "az", "ns13.intrans.az"),
    ("65.22.38.17", "bet", "c0.nic.bet"),
    ("185.24.64.85", "bond", "b.nic.bond"),
    ("213.248.218.63", "call", "dns1.nic.call"),
    ("194.169.218.78", "ceo", "a.nic.ceo"),
    ("190.124.27.10", "cl", "a.nic.cl"),
    ("200.107.82.100", "cr", "dns.nic.cr"),
    ("65.22.26.61", "degree", "v0n2.nic.degree"),
    ("65.22.26.21", "doctor", "v0n2.nic.doctor"),
    ("65.22.33.21", "equipment", "v0n1.nic.equipment"),
    ("65.22.34.8", "farm", "v0n2.nic.farm"),
    ("194.169.218.26", "fm", "a.nic.fm"),
    ("65.22.27.8", "gallery", "v2n0.nic.gallery"),
    ("65.22.28.46", "golf", "v0n0.nic.golf"),
    ("65.22.27.20", "haus", "v2n0.nic.haus"),
    ("65.22.24.7", "house", "v0n0.nic.house"),
    ("65.22.84.33", "java", "a0.nic.java"),
    ("203.189.128.1", "kh", "dns1.online.com.kh"),
    ("185.91.97.18", "lb", "nabil.ns.lb"),
    ("194.169.218.157", "llp", "a.nic.llp"),
    ("185.24.64.116", "makeup", "b.nic.makeup"),
    ("165.90.218.166", "ml", "b.nic.ml"),
    ("204.61.216.131", "mw", "pch1.nic.mw"),
]
default_glue_ips: set[str] = set()
for ip, _tld, _owner in glue:
    if len(default_glue_ips) >= 100:
        break
    if ip in default_glue_ips or ip in ("198.41.0.4", "192.58.128.30"):
        continue
    default_glue_ips.add(ip)
seen_extra_glue_ips: set[str] = set()
for ip, tld, owner in EXTRA_GLUE:
    if (ip, tld, owner) not in {(gip, gtld, gowner) for gip, gtld, gowner in glue}:
        sys.exit(f"EXTRA_GLUE {(ip, tld, owner)!r} is not root-zone glue")
    if ip in default_glue_ips or ip in seen_extra_glue_ips:
        sys.exit(f"EXTRA_GLUE repeats network anchor IP {ip!r}")
    seen_extra_glue_ips.add(ip)

# ------------------------------------------------------------- curated sets ---

# (id, name, host, sector). Real, stable, public endpoints. www is included
# where that is the canonical name; a 403/429 is a "blocked" observation, not a
# fabricated outage, so bot-walled services are still honest targets.
#
# Forty-two services that stayed bot-walled from every vantage were replaced by
# real US hub endpoints: official airport sites and city/state portals. Their
# ids are retained so their original history is preserved.
# Names use "<City>, <ST> hub — <site>" and sector "us/hub"; /hubs groups on
# that convention without changing the schema. Every replacement returned an
# assessable 200 from the egress vantage before it was added. Hosts that still
# challenge the probe were left out rather than approached with a new identity
# (docs/ACCEPTABLE_USE.md).
#
# A second pass moved stable ids between service sectors to thicken airline,
# education, health and social, which had too few targets for a stable view.
# Every moved slot still names a real public endpoint
# and the sector is still derived from this curated list, not a new city/state
# field.
SERVICES: list[tuple[str, str, str, str]] = [
    # financial / banking
    ("chase", "Chase", "www.chase.com", "financial/banking"),
    ("bofa", "Bank of America", "www.bankofamerica.com", "financial/banking"),
    ("wellsfargo", "Wells Fargo", "www.wellsfargo.com", "financial/banking"),
    ("citibank", "Citibank", "www.citibank.com", "financial/banking"),
    ("usbank", "U.S. Bank", "www.usbank.com", "financial/banking"),
    ("pnc", "PNC", "www.pnc.com", "financial/banking"),
    ("capitalone", "Capital One", "www.capitalone.com", "financial/banking"),
    ("td", "TD", "www.td.com", "financial/banking"),
    ("schwab", "Charles Schwab", "www.schwab.com", "financial/banking"),
    ("fidelity", "Fidelity", "www.fidelity.com", "financial/banking"),
    ("vanguard", "Vanguard", "www.vanguard.com", "financial/banking"),
    ("mastercard", "Dallas, TX hub — DFW airport", "www.dfwairport.com", "us/hub"),
    ("visa", "Visa", "www.visa.com", "financial/banking"),
    ("paypal", "New York, NY hub — LGA airport", "www.laguardiaairport.com", "us/hub"),
    ("stripe", "Stripe", "stripe.com", "financial/banking"),
    ("coinbase", "Coinbase", "www.coinbase.com", "financial/banking"),
    ("goldmansachs", "Goldman Sachs", "www.goldmansachs.com", "financial/banking"),
    ("morganstanley", "Morgan Stanley", "www.morganstanley.com", "financial/banking"),
    ("hsbc", "HSBC", "www.hsbc.com", "financial/banking"),
    ("barclays", "Barclays", "www.barclays.co.uk", "financial/banking"),
    ("lloyds", "Lloyds Bank", "www.lloydsbank.com", "financial/banking"),
    ("santander", "Newark, NJ hub — EWR airport", "www.newarkairport.com", "us/hub"),
    ("deutschebank", "Deutsche Bank", "www.deutsche-bank.de", "financial/banking"),
    ("bnpparibas", "San Francisco, CA hub — SFO airport", "www.flysfo.com", "us/hub"),
    ("bbva", "BBVA", "www.bbva.com", "financial/banking"),
    ("nordea", "Nordea", "www.nordea.com", "financial/banking"),
    # Two banking slots moved to airline/travel to thicken a thin sector. The
    # ids stay stable, so the rest of the fleet keeps its history.
    ("rbc", "Trip.com", "www.trip.com", "airline"),
    ("cibc", "CIBC", "www.cibc.com", "financial/banking"),
    # Turkish Airlines ends in HTTP/2 INTERNAL_ERROR from the field-trial egress
    # and all six Tor exits. Finnair is another real airline whose public page
    # answers with assessable HTML from every vantage. The id stays stable.
    ("scotiabank", "Finnair", "www.finnair.com", "airline"),
    ("westpac", "Westpac", "www.westpac.com.au", "financial/banking"),
    ("commbank", "CommBank", "www.commbank.com.au", "financial/banking"),
    ("anz", "Seattle, WA hub — SEA airport", "www.portseattle.org", "us/hub"),
    ("nab", "NAB", "www.nab.com.au", "financial/banking"),
    ("dbs", "DBS", "www.dbs.com.sg", "financial/banking"),
    ("ocbc", "Miami, FL hub — MIA airport", "www.miami-airport.com", "us/hub"),
    # airline / travel. Six services that answered with a bot challenge or a
    # config error from every vantage were swapped for other real airline and
    # travel sites that return assessable HTML. The ids are stable keys, so
    # their history survives.
    ("united", "Skyscanner", "www.skyscanner.com", "airline"),
    ("delta", "WestJet", "www.westjet.com", "airline"),
    ("american", "Houston, TX hub — IAH airport", "www.flyiah.com", "us/hub"),
    ("southwest", "Southwest", "www.southwest.com", "airline"),
    ("jetblue", "JetBlue", "www.jetblue.com", "airline"),
    ("alaska", "Alaska Airlines", "www.alaskaair.com", "airline"),
    ("lufthansa", "Minneapolis, MN hub — MSP airport", "www.mspairport.com", "us/hub"),
    # British Airways' apex times out awaiting HTTP/2 response headers from
    # three of the six Tor exits (DE, JP and SG answer; CL, AU and MX do not),
    # on both / and /robots.txt, so a path override cannot fix it. Ethiopian
    # Airlines is another real public airline page that returned 206 with
    # assessable HTML from all six exits. The id stays stable for history.
    ("britishairways", "Ethiopian Airlines", "www.ethiopianairlines.com", "airline"),
    # Singapore Airlines' canonical root now returns 404 from the field-trial
    # egress; Scoot is another real Singapore-based airline with assessable HTML.
    ("airfrance", "Scoot", "www.scoot.com", "airline"),
    ("klm", "Virgin Australia", "www.virginaustralia.com", "airline"),
    ("emirates", "Virgin Atlantic", "www.virginatlantic.com", "airline"),
    ("qatar", "Detroit, MI hub — DTW airport", "www.metroairport.com", "us/hub"),
    ("singaporeair", "Hawaiian Airlines", "www.hawaiianairlines.com", "airline"),
    ("cathay", "Cathay Pacific", "www.cathaypacific.com", "airline"),
    ("ana", "ANA", "www.ana.co.jp", "airline"),
    ("jal", "Salt Lake City, UT hub — SLC airport", "slcairport.com", "us/hub"),
    ("qantas", "Qantas", "www.qantas.com", "airline"),
    ("aircanada", "Phoenix, AZ hub — PHX airport", "www.skyharbor.com", "us/hub"),
    # Spirit ceased operations in 2026: www.spirit.com is NXDOMAIN worldwide, so
    # no vantage can reach it. Sun Country and Air Transat answer with Incapsula
    # challenges (the latter still sends x-iinfo on an otherwise clean page), so
    # a real airline that publishes assessable HTML is used here. The id is
    # retained as the stable key so this target's
    # history records the replacement.
    ("spirit", "Sunwing", "www.sunwing.ca", "airline"),
    ("frontier", "Raleigh, NC hub — RDU airport", "www.rdu.com", "us/hub"),
    ("easyjet", "Las Vegas, NV hub — LAS airport", "www.harryreidairport.com", "us/hub"),
    ("ryanair", "Ryanair", "www.ryanair.com", "airline"),
    ("airbnb", "Airbnb", "www.airbnb.com", "airline"),
    ("booking", "Booking.com", "www.booking.com", "airline"),
    ("expedia", "Washington, DC hub — IAD airport", "www.flydulles.com", "us/hub"),
    # cloud / infrastructure
    ("aws", "AWS", "aws.amazon.com", "cloud"),
    ("cloudflare", "Cloudflare", "www.cloudflare.com", "cloud"),
    ("github", "GitHub", "github.com", "developer"),
    ("microsoft", "Microsoft", "www.microsoft.com", "cloud"),
    ("azure", "Azure", "azure.microsoft.com", "cloud"),
    ("gcp", "Google Cloud", "cloud.google.com", "cloud"),
    ("ibm", "IBM", "www.ibm.com", "cloud"),
    ("oracle", "Orlando, FL hub — MCO airport", "flymco.com", "us/hub"),
    ("digitalocean", "DigitalOcean", "www.digitalocean.com", "cloud"),
    ("linode", "Linode", "www.linode.com", "cloud"),
    ("hetzner", "Hetzner", "www.hetzner.com", "cloud"),
    ("ovhcloud", "OVHcloud", "www.ovhcloud.com", "cloud"),
    ("fastly", "Fastly", "www.fastly.com", "cloud"),
    ("akamai", "Baltimore, MD hub — BWI airport", "bwiairport.com", "us/hub"),
    ("salesforce", "Salesforce", "www.salesforce.com", "cloud"),
    ("sap", "Portland, OR hub — PDX airport", "www.flypdx.com", "us/hub"),
    ("vmware", "VMware", "www.vmware.com", "cloud"),
    ("cisco", "Cisco", "www.cisco.com", "cloud"),
    ("dell", "Dell", "www.dell.com", "cloud"),
    # HPE times out awaiting HTTP/2 response headers. Oracle Cloud answers 206
    # with assessable HTML from the local egress and all six Tor exits. This is a
    # different host from the existing svc-oracle airport target, so every
    # target keeps a distinct (host, port, probe, qname) identity.
    ("hpe", "Oracle Cloud", "cloud.oracle.com", "cloud"),
    ("intel", "Intel", "www.intel.com", "cloud"),
    ("nvidia", "NVIDIA", "www.nvidia.com", "cloud"),
    ("apple", "Apple", "www.apple.com", "cloud"),
    ("google", "Google", "www.google.com", "cloud"),
    ("mozilla", "Mozilla", "www.mozilla.org", "cloud"),
    ("redhat", "Red Hat", "www.redhat.com", "developer"),
    ("canonical", "Canonical", "www.canonical.com", "developer"),
    ("docker", "Docker", "www.docker.com", "developer"),
    # social / media
    # Facebook is bot-walled from the field-trial egress. Flickr is a real public
    # social endpoint that answers 206 from the local egress and all six exits.
    # The id stays stable so the target keeps its history.
    ("facebook", "Flickr", "www.flickr.com", "social"),
    ("instagram", "Instagram", "www.instagram.com", "social"),
    ("x", "X", "x.com", "social"),
    ("linkedin", "LinkedIn", "www.linkedin.com", "social"),
    ("tiktok", "TikTok", "www.tiktok.com", "social"),
    ("youtube", "YouTube", "www.youtube.com", "social"),
    ("reddit", "Reddit", "www.reddit.com", "social"),
    ("pinterest", "Pinterest", "www.pinterest.com", "social"),
    ("snapchat", "Snapchat", "www.snapchat.com", "social"),
    ("whatsapp", "WhatsApp", "www.whatsapp.com", "social"),
    ("telegram", "Telegram", "www.telegram.org", "social"),
    ("discord", "Discord", "www.discord.com", "social"),
    ("tumblr", "Tumblr", "www.tumblr.com", "social"),
    ("quora", "Quora", "www.quora.com", "social"),
    ("medium", "Houston, TX hub — city portal", "www.houstontx.gov", "us/hub"),
    # e-commerce
    ("amazon", "Amazon", "www.amazon.com", "retail"),
    ("ebay", "Birmingham, AL hub — BHM airport", "www.flybirmingham.com", "us/hub"),
    ("walmart", "Pittsburgh, PA hub — PIT airport", "flypittsburgh.com", "us/hub"),
    ("target", "Target", "www.target.com", "retail"),
    ("costco", "Costco", "www.costco.com", "retail"),
    # Best Buy ends in HTTP/2 stream errors. Sam's Club and Walmart Canada
    # answer 200 to a normal probe but Akamai returns 418 to netweather's
    # identifying User-Agent with the explicit :443 Host that targetURL builds.
    # Dollarama answers 200 from the local egress and all six Tor exits.
    # Stable id, retail sector unchanged.
    ("bestbuy", "Dollarama", "www.dollarama.com", "retail"),
    ("homedepot", "Memphis, TN hub — MEM airport", "flymemphis.com", "us/hub"),
    ("lowes", "Lowe's", "www.lowes.com", "retail"),
    ("macys", "New Orleans, LA hub — MSY airport", "flymsy.com", "us/hub"),
    ("nordstrom", "Nordstrom", "www.nordstrom.com", "retail"),
    ("shopify", "Shopify", "www.shopify.com", "retail"),
    ("etsy", "Louisville, KY hub — SDF airport", "www.flylouisville.com", "us/hub"),
    ("aliexpress", "AliExpress", "www.aliexpress.com", "retail"),
    ("alibaba", "Alibaba", "www.alibaba.com", "retail"),
    ("taobao", "Taobao", "www.taobao.com", "retail"),
    ("jd", "JD.com", "www.jd.com", "retail"),
    ("rakuten", "Rakuten", "www.rakuten.com", "retail"),
    ("wayfair", "Wayfair", "www.wayfair.com", "retail"),
    ("zara", "Savannah, GA hub — SAV airport", "savannahairport.com", "us/hub"),
    # Uniqlo also fails HTTP/2 stream setup. Barnes & Noble answers 200 from
    # the local egress and all six Tor exits with the production UA and URL
    # shape. Stable id, retail sector unchanged.
    ("uniqlo", "Barnes & Noble", "www.barnesandnoble.com", "retail"),
    ("nike", "Nike", "www.nike.com", "retail"),
    ("adidas", "Chattanooga, TN hub — CHA airport", "www.flycha.com", "us/hub"),
    # news / reference content
    ("wikipedia", "Wikipedia", "www.wikipedia.org", "news"),
    ("hn", "Hacker News", "news.ycombinator.com", "news"),
    ("reuters", "Phoenix, AZ hub — city portal", "www.phoenix.gov", "us/hub"),
    ("bbc", "BBC", "www.bbc.com", "news"),
    ("cnn", "CNN", "www.cnn.com", "news"),
    ("nytimes", "Boston, MA hub — city portal", "www.boston.gov", "us/hub"),
    ("guardian", "The Guardian", "www.theguardian.com", "news"),
    # The Washington Post times out behind a bot wall. CBS News answers 206 with
    # assessable HTML from the local egress and all six Tor exits. Id stable.
    ("washingtonpost", "CBS News", "www.cbsnews.com", "news"),
    ("wsj", "Seattle, WA hub — city portal", "www.seattle.gov", "us/hub"),
    ("ft", "Financial Times", "www.ft.com", "news"),
    ("npr", "NPR", "www.npr.org", "news"),
    ("ap", "San Francisco, CA hub — city portal", "www.sf.gov", "us/hub"),
    ("aljazeera", "Al Jazeera", "www.aljazeera.com", "news"),
    ("dw", "Deutsche Welle", "www.dw.com", "news"),
    ("theverge", "The Verge", "www.theverge.com", "news"),
    ("techcrunch", "TechCrunch", "www.techcrunch.com", "news"),
    ("wired", "WIRED", "www.wired.com", "news"),
    ("arstechnica", "Ars Technica", "www.arstechnica.com", "news"),
    # government / education
    ("usagov", "USA.gov", "www.usa.gov", "government"),
    ("govuk", "GOV.UK", "www.gov.uk", "government"),
    ("canadaca", "Canada.ca", "www.canada.ca", "government"),
    ("europa", "European Union", "www.europa.eu", "government"),
    ("un", "United Nations", "www.un.org", "government"),
    ("nato", "NATO", "www.nato.int", "government"),
    ("who", "WHO", "www.who.int", "government"),
    ("worldbank", "World Bank", "www.worldbank.org", "government"),
    ("imf", "ITU", "www.itu.int", "government"),
    ("oecd", "OECD", "www.oecd.org", "government"),
    ("nist", "NIST", "www.nist.gov", "government"),
    ("nasa", "NASA", "www.nasa.gov", "government"),
    ("noaa", "New York, NY hub — JFK airport", "www.jfkairport.com", "us/hub"),
    ("mit", "MIT", "www.mit.edu", "education"),
    ("stanford", "Stanford", "www.stanford.edu", "education"),
    ("harvard", "Harvard", "www.harvard.edu", "education"),
    ("oxford", "Oxford", "www.ox.ac.uk", "education"),
    ("cambridge", "Cambridge", "www.cam.ac.uk", "education"),
    # healthcare
    ("cdc", "CDC", "www.cdc.gov", "health"),
    ("nih", "NIH", "www.nih.gov", "health"),
    ("fda", "FDA", "www.fda.gov", "health"),
    ("mayoclinic", "San Diego, CA hub — city portal", "www.sandiego.gov", "us/hub"),
    ("clevelandclinic", "Cleveland Clinic", "www.clevelandclinic.org", "health"),
    ("hopkins", "Johns Hopkins Medicine", "www.hopkinsmedicine.org", "health"),
    ("nhs", "NHS", "www.nhs.uk", "health"),
    ("healthline", "Healthline", "www.healthline.com", "health"),
    ("webmd", "WebMD", "www.webmd.com", "health"),
    # telecom
    ("att", "AT&T", "www.att.com", "telecom"),
    ("verizon", "Verizon", "www.verizon.com", "telecom"),
    ("tmobile", "T-Mobile", "www.t-mobile.com", "telecom"),
    ("comcast", "Los Angeles, CA hub — city portal", "lacity.gov", "us/hub"),
    ("spectrum", "Telstra", "www.telstra.com.au", "telecom"),
    ("bt", "Salt Lake City, UT hub — city portal", "www.slc.gov", "us/hub"),
    ("vodafone", "Vodafone", "www.vodafone.com", "telecom"),
    ("orange", "Orange", "www.orange.com", "telecom"),
    ("telefonica", "Telefónica", "www.telefonica.com", "telecom"),
    ("docomo", "NTT Docomo", "www.nttdocomo.co.jp", "telecom"),
    ("sktelecom", "SK Telecom", "www.sktelecom.com", "telecom"),
    # developer / SaaS
    ("gitlab", "GitLab", "gitlab.com", "developer"),
    ("bitbucket", "Bitbucket", "bitbucket.org", "developer"),
    ("atlassian", "Atlassian", "www.atlassian.com", "developer"),
    ("slack", "Slack", "www.slack.com", "developer"),
    ("zoom", "American Diabetes Association", "www.diabetes.org", "health"),
    ("notion", "Notion", "www.notion.so", "developer"),
    ("figma", "Figma", "www.figma.com", "developer"),
    ("canva", "Canva", "www.canva.com", "developer"),
    ("dropbox", "Dropbox", "www.dropbox.com", "developer"),
    ("openai", "NYU Langone Health", "www.nyulangone.org", "health"),
    ("anthropic", "UCSF Health", "www.ucsfhealth.org", "health"),
    ("huggingface", "American Cancer Society", "www.cancer.org", "health"),
    ("npm", "npm", "www.npmjs.com", "developer"),
    ("kubernetes", "Kubernetes", "www.kubernetes.io", "developer"),
    ("nginx", "NGINX", "www.nginx.com", "developer"),
    ("redis", "Redis", "www.redis.io", "developer"),
    ("postgresql", "PostgreSQL", "www.postgresql.org", "developer"),
    ("mysql", "Portland, OR hub — city portal", "www.portland.gov", "us/hub"),
    ("python", "Python", "www.python.org", "developer"),
    ("golang", "Go", "www.golang.org", "developer"),
    ("rust", "Rust", "www.rust-lang.org", "developer"),
    # additional public endpoints to bring the service tier to 240
    ("bing", "Bing", "www.bing.com", "news"),
    ("yahoo", "Yahoo", "www.yahoo.com", "news"),
    ("duckduckgo", "DuckDuckGo", "duckduckgo.com", "news"),
    ("brave", "Mass General Brigham", "www.massgeneral.org", "health"),
    ("startpage", "Startpage", "www.startpage.com", "news"),
    ("ecosia", "Ecosia", "www.ecosia.org", "news"),
    ("baidu", "Baidu", "www.baidu.com", "news"),
    ("yandex", "Mastodon", "mastodon.social", "social"),
    ("naver", "Bluesky", "bsky.app", "social"),
    ("seznam", "Seznam", "www.seznam.cz", "news"),
    ("mailru", "Mail.ru", "mail.ru", "news"),
    ("zhihu", "Chicago, IL hub — city portal", "www.chicago.gov", "us/hub"),
    ("bilibili", "Bilibili", "www.bilibili.com", "social"),
    ("tmall", "Tmall", "www.tmall.com", "retail"),
    ("pinduoduo", "Pinduoduo", "www.pinduoduo.com", "retail"),
    ("meituan", "Meituan", "www.meituan.com", "retail"),
    ("ctrip", "Ctrip", "www.ctrip.com", "retail"),
    ("suning", "Suning", "www.suning.com", "retail"),
    ("mi", "Xiaomi", "www.mi.com", "cloud"),
    # Nine hardware/vendor slots were reassigned to universities. They were
    # already well represented in cloud; education had only five targets, so
    # a 100%-measured sector was still too thin to support a verdict. Ids are
    # stable for the original fleet.
    ("huawei", "UT Austin", "www.utexas.edu", "education"),
    ("oppo", "Cornell University", "www.cornell.edu", "education"),
    ("vivo", "Yale University", "www.yale.edu", "education"),
    ("oneplus", "University of Chicago", "www.uchicago.edu", "education"),
    ("samsung", "UC Berkeley", "www.berkeley.edu", "education"),
    ("lg", "UCLA", "www.ucla.edu", "education"),
    ("sony", "Cleveland, OH hub — CLE airport", "www.clevelandairport.com", "us/hub"),
    ("lenovo", "Caltech", "www.caltech.edu", "education"),
    ("asus", "Duke University", "www.duke.edu", "education"),
    ("acer", "University of Toronto", "www.utoronto.ca", "education"),
    ("msi", "Oklahoma City, OK hub — OKC airport", "flyokc.com", "us/hub"),
    ("gigabyte", "St. Louis, MO hub — STL airport", "www.flystl.com", "us/hub"),
    ("ikea", "IKEA", "www.ikea.com", "retail"),
    ("carrefour", "Boise, ID hub — BOI airport", "www.flyboise.com", "us/hub"),
    ("tesco", "Austin, TX hub — city portal", "www.austintexas.gov", "us/hub"),
    ("sainsburys", "Denver, CO hub — city portal", "www.denvergov.org", "us/hub"),
    ("mercadolibre", "Mercado Libre", "www.mercadolibre.com", "retail"),
    ("latam", "Washington, DC hub — DCA airport", "www.flyreagan.com", "us/hub"),
    ("airnewzealand", "Air New Zealand", "www.airnewzealand.co.nz", "airline"),
    # Fleet expansion: real public endpoints verified from the field-trial
    # egress with the documented netweather User-Agent. 200 responses had to
    # carry assessable content; bounded 206 range responses were accepted too.
    # Every host was checked for bot-challenge markers and no challenged host
    # was retried with a different identity (docs/ACCEPTABLE_USE.md).
    ("rogers", "Rogers", "www.rogers.com", "telecom"),
    ("bellcanada", "Bell Canada", "www.bell.ca", "telecom"),
    ("americamovil", "América Móvil", "www.americamovil.com", "telecom"),
    ("movistar", "Movistar", "www.movistar.com", "telecom"),
    ("timbrasil", "TIM Brasil", "www.tim.com.br", "telecom"),
    ("entel", "Entel", "www.entel.cl", "telecom"),
    ("personalar", "Personal", "www.personal.com.ar", "telecom"),
    ("vodafoneuk", "Vodafone UK", "www.vodafone.co.uk", "telecom"),
    ("ee", "EE", "www.ee.co.uk", "telecom"),
    ("threeuk", "Three UK", "www.three.co.uk", "telecom"),
    ("deutschetelekom", "Deutsche Telekom", "www.telekom.de", "telecom"),
    ("kpn", "KPN", "www.kpn.com", "telecom"),
    ("proximus", "Proximus", "www.proximus.be", "telecom"),
    ("swisscom", "Swisscom", "www.swisscom.ch", "telecom"),
    ("orangefr", "Orange France", "www.orange.fr", "telecom"),
    ("bouygues", "Bouygues Telecom", "www.bouyguestelecom.fr", "telecom"),
    ("sfr", "SFR", "www.sfr.fr", "telecom"),
    ("windtre", "Wind Tre", "www.windtre.it", "telecom"),
    ("timitalia", "TIM Italia", "www.tim.it", "telecom"),
    ("a1austria", "A1 Austria", "www.a1.at", "telecom"),
    ("telenor", "Telenor", "www.telenor.no", "telecom"),
    ("telia", "Telia", "www.telia.se", "telecom"),
    ("elisa", "Elisa", "www.elisa.fi", "telecom"),
    ("tdcdk", "TDC", "www.tdc.dk", "telecom"),
    ("playpl", "Play", "www.play.pl", "telecom"),
    ("a1bulgaria", "A1 Bulgaria", "www.a1.bg", "telecom"),
    ("turktelekom", "Turk Telekom", "www.turktelekom.com.tr", "telecom"),
    ("turkcell", "Turkcell", "www.turkcell.com.tr", "telecom"),
    ("mtsru", "MTS", "www.mts.ru", "telecom"),
    ("kddi", "KDDI", "www.kddi.com", "telecom"),
    ("kt", "KT", "www.kt.com", "telecom"),
    ("cht", "Chunghwa Telecom", "www.cht.com.tw", "telecom"),
    ("tmmalaysia", "Telekom Malaysia", "www.tm.com.my", "telecom"),
    ("maxis", "Maxis", "www.maxis.com.my", "telecom"),
    ("singtel", "Singtel", "www.singtel.com", "telecom"),
    ("pldt", "PLDT", "www.pldt.com", "telecom"),
    ("telkomid", "Telkom Indonesia", "www.telkom.co.id", "telecom"),
    ("airtelin", "Airtel India", "www.airtel.in", "telecom"),
    ("etisalat", "Etisalat", "www.etisalat.ae", "telecom"),
    ("du", "du", "www.du.ae", "telecom"),
    ("stc", "stc", "www.stc.com.sa", "telecom"),
    ("mtn", "MTN", "www.mtn.com", "telecom"),
    ("vodafoneau", "Vodafone Australia", "www.vodafone.com.au", "telecom"),
    ("tpg", "TPG", "www.tpg.com.au", "telecom"),
    ("australiagov", "Australian Government", "www.australia.gov.au", "government"),
    ("govtnz", "New Zealand Government", "www.govt.nz", "government"),
    ("govsg", "Singapore Government", "www.gov.sg", "government"),
    ("govbr", "Government of Brazil", "www.gov.br", "government"),
    ("gobmx", "Government of Mexico", "www.gob.mx", "government"),
    ("govza", "Government of South Africa", "www.gov.za", "government"),
    ("ec", "European Commission", "ec.europa.eu", "government"),
    ("unicef", "UNICEF", "www.unicef.org", "government"),
    ("icrc", "ICRC", "www.icrc.org", "government"),
    ("ethz", "ETH Zürich", "www.ethz.ch", "education"),
    ("epfl", "EPFL", "www.epfl.ch", "education"),
    ("kyoto", "Kyoto University", "www.kyoto-u.ac.jp", "education"),
    ("snu", "Seoul National University", "www.snu.ac.kr", "education"),
    ("ntu", "NTU Singapore", "www.ntu.edu.sg", "education"),
    ("hku", "University of Hong Kong", "www.hku.hk", "education"),
    ("unimelb", "University of Melbourne", "www.unimelb.edu.au", "education"),
    ("sydneyu", "University of Sydney", "www.sydney.edu.au", "education"),
    ("uct", "University of Cape Town", "www.uct.ac.za", "education"),
    ("uonbi", "University of Nairobi", "www.uonbi.ac.ke", "education"),
    ("usp", "Universidade de São Paulo", "www.usp.br", "education"),
    ("mcgill", "McGill", "www.mcgill.ca", "education"),
    ("ubc", "UBC", "www.ubc.ca", "education"),
    ("tum", "Technical University of Munich", "www.tum.de", "education"),
    ("uva", "University of Amsterdam", "www.uva.nl", "education"),
    ("sorbonne", "Sorbonne Université", "www.sorbonne-universite.fr", "education"),
    ("lung", "American Lung Association", "www.lung.org", "health"),
    ("stanfordhealth", "Stanford Health Care", "www.stanfordhealthcare.org", "health"),
    ("mercy", "Mercy", "www.mercy.net", "health"),
    ("kaiser", "Kaiser Permanente", "www.kp.org", "health"),
    ("cvshealth", "CVS Health", "www.cvshealth.com", "health"),
    ("walgreens", "Walgreens", "www.walgreens.com", "health"),
    ("ema", "European Medicines Agency", "www.ema.europa.eu", "health"),
    ("msf", "Médecins Sans Frontières", "www.msf.org", "health"),
    ("stjude", "St. Jude", "www.stjude.org", "health"),
    ("dana", "Dana-Farber", "www.dana-farber.org", "health"),
    ("mskcc", "Memorial Sloan Kettering", "www.mskcc.org", "health"),
    ("mdanderson", "MD Anderson", "www.mdanderson.org", "health"),
    ("ally", "Ally", "www.ally.com", "financial/banking"),
    ("discover", "Discover", "www.discover.com", "financial/banking"),
    ("amex", "American Express", "www.americanexpress.com", "financial/banking"),
    ("scotiabankca", "Scotiabank", "www.scotiabank.com", "financial/banking"),
    ("nbc", "National Bank of Canada", "www.nbc.ca", "financial/banking"),
    ("caixabank", "CaixaBank", "www.caixabank.es", "financial/banking"),
    ("ing", "ING", "www.ing.com", "financial/banking"),
    ("ubs", "UBS", "www.ubs.com", "financial/banking"),
    ("uob", "UOB", "www.uobgroup.com", "financial/banking"),
    ("cimb", "CIMB", "www.cimb.com", "financial/banking"),
    ("icbc", "ICBC", "www.icbc.com.cn", "financial/banking"),
    ("boc", "Bank of China", "www.boc.cn", "financial/banking"),
    ("mufg", "MUFG", "www.mufg.jp", "financial/banking"),
    ("smbc", "SMBC", "www.smbc.co.jp", "financial/banking"),
    ("kebhana", "Hana Bank", "www.kebhana.com", "financial/banking"),
    ("standardbank", "Standard Bank", "www.standardbank.com", "financial/banking"),
    ("nedbank", "Nedbank", "www.nedbank.co.za", "financial/banking"),
    ("absa", "Absa", "www.absa.co.za", "financial/banking"),
    ("vercel", "Vercel", "vercel.com", "cloud"),
    ("netlify", "Netlify", "www.netlify.com", "cloud"),
    ("heroku", "Heroku", "www.heroku.com", "cloud"),
    ("render", "Render", "render.com", "cloud"),
    ("fly", "Fly.io", "fly.io", "cloud"),
    ("scaleway", "Scaleway", "www.scaleway.com", "cloud"),
    ("alibabacloud", "Alibaba Cloud", "www.alibabacloud.com", "cloud"),
    ("tencentcloud", "Tencent Cloud", "www.tencentcloud.com", "cloud"),
    ("huaweicloud", "Huawei Cloud", "www.huaweicloud.com", "cloud"),
    ("jetbrains", "JetBrains", "www.jetbrains.com", "developer"),
    ("sourceforge", "SourceForge", "sourceforge.net", "developer"),
    ("gitea", "Gitea", "gitea.io", "developer"),
    ("elastic", "Elastic", "www.elastic.co", "developer"),
    ("grafana", "Grafana", "grafana.com", "developer"),
    ("datadog", "Datadog", "www.datadoghq.com", "developer"),
    ("supabase", "Supabase", "supabase.com", "developer"),
    ("mongodb", "MongoDB", "www.mongodb.com", "developer"),
    ("suse", "SUSE", "www.suse.com", "developer"),
    ("fedora", "Fedora", "getfedora.org", "developer"),
    ("debian", "Debian", "www.debian.org", "developer"),
    ("ubuntu", "Ubuntu", "www.ubuntu.com", "developer"),
    ("archlinux", "Arch Linux", "archlinux.org", "developer"),
    ("xinhua", "Xinhua", "www.xinhuanet.com", "news"),
    ("kyodo", "Kyodo News", "www.kyodonews.jp", "news"),
    ("yonhap", "Yonhap", "www.yna.co.kr", "news"),
    ("anadolu", "Anadolu Agency", "www.aa.com.tr", "news"),
    ("pti", "Press Trust of India", "www.ptinews.com", "news"),
    ("nhk", "NHK", "www3.nhk.or.jp", "news"),
    ("france24", "France 24", "www.france24.com", "news"),
    ("cna", "CNA", "www.channelnewsasia.com", "news"),
    ("japantimes", "The Japan Times", "www.japantimes.co.jp", "news"),
    ("koreaherald", "The Korea Herald", "www.koreaherald.com", "news"),
    ("ctv", "CTV News", "www.ctvnews.ca", "news"),
    ("elpais", "El País", "elpais.com", "news"),
    ("corriere", "Corriere della Sera", "www.corriere.it", "news"),
    ("folha", "Folha de S.Paulo", "www.folha.uol.com.br", "news"),
    ("clarin", "Clarín", "www.clarin.com", "news"),
    ("aldi", "Aldi", "www.aldi.com", "retail"),
    ("lidl", "Lidl", "www.lidl.com", "retail"),
    ("kmart", "Kmart", "www.kmart.com", "retail"),
    ("ulta", "Ulta Beauty", "www.ulta.com", "retail"),
    ("decathlon", "Decathlon", "www.decathlon.com", "retail"),
    ("flipkart", "Flipkart", "www.flipkart.com", "retail"),
    ("lazada", "Lazada", "www.lazada.com", "retail"),
    ("shopee", "Shopee", "www.shopee.com", "retail"),
    ("tap", "TAP Air Portugal", "www.flytap.com", "airline"),
    ("gulfair", "Gulf Air", "www.gulfair.com", "airline"),
    ("kuwaitairways", "Kuwait Airways", "www.kuwaitairways.com", "airline"),
    ("kenyaairways", "Kenya Airways", "www.kenya-airways.com", "airline"),
    ("rwandair", "RwandAir", "www.rwandair.com", "airline"),
    ("saairways", "South African Airways", "www.flysaa.com", "airline"),
    ("volaris", "Volaris", "www.volaris.com", "airline"),
    ("gol", "GOL", "www.voegol.com.br", "airline"),
    ("chinaairlines", "China Airlines", "www.china-airlines.com", "airline"),
    ("ceair", "China Eastern", "www.ceair.com", "airline"),
    ("csair", "China Southern", "www.csair.com", "airline"),
    ("spicejet", "SpiceJet", "www.spicejet.com", "airline"),
    ("akasa", "Akasa Air", "www.akasaair.com", "airline"),
    ("okru", "Odnoklassniki", "ok.ru", "social"),
    ("line", "LINE", "line.me", "social"),
    ("viber", "Viber", "www.viber.com", "social"),
    ("signal", "Signal", "signal.org", "social"),
    ("threads", "Threads", "www.threads.net", "social"),
    ("mewe", "MeWe", "www.mewe.com", "social"),
    ("minds", "Minds", "www.minds.com", "social"),
    ("diaspora", "Diaspora", "diasporafoundation.org", "social"),
    ("pixelfed", "Pixelfed", "pixelfed.org", "social"),
]

REFERENCES = [
    ("ref-example", "example.com", "example.com", "reference"),
    ("ref-doh-google", "Google DoH", "dns.google", "reference"),
    ("ref-doh-cloudflare", "Cloudflare DoH", "cloudflare-dns.com", "reference"),
    ("ref-doh-quad9", "Quad9 DoH", "dns9.quad9.net", "reference"),
    ("ref-doh-adguard", "AdGuard DoH", "dns.adguard-dns.com", "reference"),
    ("ref-doh-opendns", "OpenDNS DoH", "doh.opendns.com", "reference"),
    # Root servers are named by hostname, which keeps them in the reference
    # class (the config rejects an IP literal for a non-network target).
    ("ref-root-a", "Root server A", "a.root-servers.net", "reference"),
    ("ref-root-j", "Root server J", "j.root-servers.net", "reference"),
    ("ref-root-k", "Root server K", "k.root-servers.net", "reference"),
    ("ref-root-m", "Root server M", "m.root-servers.net", "reference"),
]

# Corridors are an operator-supplied pair of real, public TCP:443 endpoints
# (DESIGN.md §9). The design deliberately ships no default list: inventing
# hostnames would fabricate the measurement. Every endpoint below was verified
# to accept a TCP connect on 443 from the field-trial host. The city labels are
# approximate because CDNs anycast — the endpoints, not the labels, are the
# measurement.
CORRIDORS: list[tuple[str, str, str, str, str]] = [
    ("nyc-lon", "NYC", "LON", "www.nytimes.com:443", "www.bbc.co.uk:443"),
    ("lon-sin", "LON", "SIN", "www.bbc.co.uk:443", "www.straitstimes.com:443"),
    ("sin-tyo", "SIN", "TYO", "www.straitstimes.com:443", "www.asahi.com:443"),
    ("lax-syd", "LAX", "SYD", "www.sfchronicle.com:443", "www.abc.net.au:443"),
    ("mrs-bom", "MRS", "BOM", "www.lemonde.fr:443", "www.thehindu.com:443"),
    ("mia-sao", "MIA", "SAO", "www.chase.com:443", "www.globo.com:443"),
    ("fra-nyc", "FRA", "NYC", "www.spiegel.de:443", "www.nytimes.com:443"),
    ("syd-sin", "SYD", "SIN", "www.commbank.com.au:443", "www.dbs.com.sg:443"),
    ("jnb-lon", "JNB", "LON", "www.news24.com:443", "www.barclays.co.uk:443"),
    ("sfo-tyo", "SFO", "TYO", "www.sfchronicle.com:443", "www.asahi.com:443"),
]

# -------------------------------------------------------------- emit config ---

def y(value: str) -> str:
    return '"' + value.replace("\\", "\\\\").replace('"', '\\"') + '"'


out: list[str] = []
w = out.append
w("# netweather early field-trial fleet")
w("# Generated by scripts/gen_fieldtrial_targets.py from the IANA root zone.")
w("# Every target has a distinct (host, port, probe, qname) identity.")
w("version: 1")
w("targets:")

counts = collections.Counter()

# Country anchors: one DNS question per ccTLD against its authoritative NS.
for tld in sorted(chosen_ns):
    ns = REACHABLE_NS.get(tld, chosen_ns[tld])
    w(f"  - id: cctld-{tld}")
    w(f"    name: {y(country_name(tld))}")
    w(f"    host: {y(ns)}")
    w("    port: 53")
    w("    probe: dns")
    w(f"    qname: {y(tld)}")
    w("    class: country")
    w("    sector: country")
    w("    country: " + y(tld.upper() if len(tld) == 2 else "XN"))
    w("    tier: 3")
    counts["country"] += 1

# A second observation per selected ccTLD, aimed at a different root-zone NS.
for tld, ns in SECONDARY_NS:
    w(f"  - id: cctld2-{tld}")
    w(f"    name: {y(NAMES.get(tld.lower(), tld.upper()) + ' ccTLD secondary')}")
    w(f"    host: {y(ns)}")
    w("    port: 53")
    w("    probe: dns")
    w(f"    qname: {y(tld)}")
    w("    class: country")
    w("    sector: country")
    w("    country: " + y(tld.upper() if len(tld) == 2 else "XN"))
    w("    tier: 3")
    counts["country"] += 1

w("")

# Network anchors: distinct authoritative DNS IPs from root-zone glue. Query
# each server about the TLD it is glue for, so the answer is meaningful.
seen_ips: set[str] = set()
for ip, tld, owner in glue:
    if len(seen_ips) >= 100:
        break
    if ip in seen_ips:
        continue
    if ip in ("198.41.0.4", "192.58.128.30"):  # already reference targets
        continue
    seen_ips.add(ip)
    slug = re.sub(r"[^a-z0-9]+", "-", owner).strip("-")[:50]
    w(f"  - id: net-{slug}")
    w(f"    name: {y(owner + ' DNS')}")
    w(f"    host: {y(ip)}")
    w("    port: 53")
    w("    probe: dns")
    w(f"    qname: {y(tld)}")
    w("    class: network")
    w("    sector: network")
    w("    tier: 3")
    counts["network"] += 1

# Extra distinct glue IPs, using new ids so the first 100 keep their history.
used_net2_ids: set[str] = set()
for ip, tld, owner in EXTRA_GLUE:
    base = re.sub(r"[^a-z0-9]+", "-", owner).strip("-")[:50]
    ident = f"net2-{base}"
    suffix = 2
    while ident in used_net2_ids:
        ident = f"net2-{base}-{suffix}"
        suffix += 1
    used_net2_ids.add(ident)
    w(f"  - id: {ident}")
    w(f"    name: {y(owner + ' DNS')}")
    w(f"    host: {y(ip)}")
    w("    port: 53")
    w("    probe: dns")
    w(f"    qname: {y(tld)}")
    w("    class: network")
    w("    sector: network")
    w("    tier: 3")
    counts["network"] += 1

w("")

# A few services need a concrete path. Cathay's apex takes ~10 s to 301 and the
# canonical page another ~10 s, so the default probe exceeds its 15 s timeout;
# asking for the final page directly keeps the same public service measurable.
# Sunwing's /en/ page is likewise the stable public page that returns
# assessable HTML without a bot challenge. Every path is verified against the
# real endpoint.
SERVICE_PATHS: dict[str, str] = {
    "cathay": "/cx/en_US.html",
    "spirit": "/en/",
    # US hub targets use the final public page rather than a redirect chain.
    "anz": "/sea",
    "sainsburys": "/Home",
    "zhihu": "/city/en.html",
}

# Service targets: curated real endpoints.
seen_hosts: set[str] = set()
for sid, name, host, sector in SERVICES:
    if host in seen_hosts:
        continue
    seen_hosts.add(host)
    w(f"  - id: svc-{sid}")
    w(f"    name: {y(name)}")
    w(f"    host: {y(host)}")
    w("    port: 443")
    w("    probe: http")
    if sid in SERVICE_PATHS:
        w(f"    path: {y(SERVICE_PATHS[sid])}")
    w("    class: service")
    w(f"    sector: {y(sector)}")
    if sector in ("financial/banking", "cloud", "developer"):
        w("    tier: 1")
        w("    interval_s: 120")
    else:
        w("    tier: 2")
        w("    interval_s: 300")
    counts["service"] += 1

w("")

# References: self-health and public DNS anchors.
for rid, name, host, cls in REFERENCES:
    is_ip = bool(re.fullmatch(r"\d{1,3}(\.\d{1,3}){3}", host))
    if is_ip or rid.startswith("ref-root-"):
        probe, port, qname = "dns", 53, "com"
    elif "doh" in rid or host.startswith("dns"):
        probe, port, qname = "doh", 443, "example.com"
    else:
        probe, port, qname = "http", 443, ""
    w(f"  - id: {rid}")
    w(f"    name: {y(name)}")
    w(f"    host: {y(host)}")
    w(f"    port: {port}")
    w(f"    probe: {probe}")
    if qname:
        w(f"    qname: {y(qname)}")
    w(f"    class: {cls}")
    w("    sector: reference")
    w("    tier: 0")
    w("    interval_s: 600")
    counts["reference"] += 1

# Operator-supplied path pairs (DESIGN.md §9). Kept separate from the target
# count: corridors are evidence about a path, never probe targets. Emit the
# same explanation into the config so regeneration preserves it.
if CORRIDORS:
    w("")
    w("# Operator-supplied path pairs (DESIGN.md §9). The design deliberately ships")
    w("# no default list: each endpoint below is a real public TCP:443 service that")
    w("# was verified to connect from the field-trial host. City labels are approximate")
    w("# because CDNs anycast — the endpoints, not the labels, are the measurement.")
    w("corridors:")
    for cid, frm, to, near, far in CORRIDORS:
        w(f"  - id: {cid}")
        w(f"    from: {frm}")
        w(f"    to: {to}")
        w(f"    endpoints: [{y(near)}, {y(far)}]")

with open(OUT, "w", encoding="utf-8", newline="\n") as f:
    f.write("\n".join(out) + "\n")

print(f"wrote {OUT}: {dict(counts)} total {sum(counts.values())}")
