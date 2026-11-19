#!/usr/bin/env python3
"""Generate the early field-trial target config for netweather.

Inputs
------
A saved IANA root zone file (the same source `netweather seed --country-anchors`
uses). Pass its path as the first argument, or set ROOT_ZONE. The root zone gives
us two real, operator-maintained inventories:

  * ccTLD NS delegations  -> ~399 country anchors (one DNS question per territory)
  * glue A records       -> ~100 network anchors (authoritative DNS server IPs)

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

body = open(ROOT_ZONE, encoding="utf-8", errors="replace").read()
lines = []
for raw in body.split("\n"):
    line = raw.split(";", 1)[0]
    fields = line.split()
    if len(fields) >= 5 and fields[0].upper() not in ("$TTL", "$ORIGIN"):
        lines.append(fields)

# ccTLD -> lexicographically first NS (matches internal/seed.ParseCountryAnchors).
chosen_ns: dict[str, str] = {}
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
    if name not in chosen_ns or ns < chosen_ns[name]:
        chosen_ns[name] = ns

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

# ------------------------------------------------------------- curated sets ---

# (id, name, host, sector). Real, stable, public endpoints. www is included
# where that is the canonical name; a 403/429 is a "blocked" observation, not a
# fabricated outage, so bot-walled services are still honest targets.
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
    ("mastercard", "Mastercard", "www.mastercard.com", "financial/banking"),
    ("visa", "Visa", "www.visa.com", "financial/banking"),
    ("paypal", "PayPal", "www.paypal.com", "financial/banking"),
    ("stripe", "Stripe", "stripe.com", "financial/banking"),
    ("coinbase", "Coinbase", "www.coinbase.com", "financial/banking"),
    ("goldmansachs", "Goldman Sachs", "www.goldmansachs.com", "financial/banking"),
    ("morganstanley", "Morgan Stanley", "www.morganstanley.com", "financial/banking"),
    ("hsbc", "HSBC", "www.hsbc.com", "financial/banking"),
    ("barclays", "Barclays", "www.barclays.co.uk", "financial/banking"),
    ("lloyds", "Lloyds Bank", "www.lloydsbank.com", "financial/banking"),
    ("santander", "Santander", "www.santander.com", "financial/banking"),
    ("deutschebank", "Deutsche Bank", "www.deutsche-bank.de", "financial/banking"),
    ("bnpparibas", "BNP Paribas", "www.bnpparibas.com", "financial/banking"),
    ("bbva", "BBVA", "www.bbva.com", "financial/banking"),
    ("nordea", "Nordea", "www.nordea.com", "financial/banking"),
    ("rbc", "Royal Bank of Canada", "www.rbc.com", "financial/banking"),
    ("cibc", "CIBC", "www.cibc.com", "financial/banking"),
    ("scotiabank", "Scotiabank", "www.scotiabank.com", "financial/banking"),
    ("westpac", "Westpac", "www.westpac.com.au", "financial/banking"),
    ("commbank", "CommBank", "www.commbank.com.au", "financial/banking"),
    ("anz", "ANZ", "www.anz.com", "financial/banking"),
    ("nab", "NAB", "www.nab.com.au", "financial/banking"),
    ("dbs", "DBS", "www.dbs.com.sg", "financial/banking"),
    ("ocbc", "OCBC", "www.ocbc.com", "financial/banking"),
    # airline / travel
    ("united", "United", "www.united.com", "airline"),
    ("delta", "Delta", "www.delta.com", "airline"),
    ("american", "American Airlines", "www.aa.com", "airline"),
    ("southwest", "Southwest", "www.southwest.com", "airline"),
    ("jetblue", "JetBlue", "www.jetblue.com", "airline"),
    ("alaska", "Alaska Airlines", "www.alaskaair.com", "airline"),
    ("lufthansa", "Lufthansa", "www.lufthansa.com", "airline"),
    ("britishairways", "British Airways", "www.britishairways.com", "airline"),
    ("airfrance", "Air France", "www.airfrance.com", "airline"),
    ("klm", "KLM", "www.klm.com", "airline"),
    ("emirates", "Emirates", "www.emirates.com", "airline"),
    ("qatar", "Qatar Airways", "www.qatarairways.com", "airline"),
    ("singaporeair", "Singapore Airlines", "www.singaporeair.com", "airline"),
    ("cathay", "Cathay Pacific", "www.cathaypacific.com", "airline"),
    ("ana", "ANA", "www.ana.co.jp", "airline"),
    ("jal", "Japan Airlines", "www.jal.co.jp", "airline"),
    ("qantas", "Qantas", "www.qantas.com", "airline"),
    ("aircanada", "Air Canada", "www.aircanada.com", "airline"),
    ("spirit", "Spirit", "www.spirit.com", "airline"),
    ("frontier", "Frontier", "www.flyfrontier.com", "airline"),
    ("easyjet", "easyJet", "www.easyjet.com", "airline"),
    ("ryanair", "Ryanair", "www.ryanair.com", "airline"),
    ("airbnb", "Airbnb", "www.airbnb.com", "airline"),
    ("booking", "Booking.com", "www.booking.com", "airline"),
    ("expedia", "Expedia", "www.expedia.com", "airline"),
    # cloud / infrastructure
    ("aws", "AWS", "aws.amazon.com", "cloud"),
    ("cloudflare", "Cloudflare", "www.cloudflare.com", "cloud"),
    ("github", "GitHub", "github.com", "developer"),
    ("microsoft", "Microsoft", "www.microsoft.com", "cloud"),
    ("azure", "Azure", "azure.microsoft.com", "cloud"),
    ("gcp", "Google Cloud", "cloud.google.com", "cloud"),
    ("ibm", "IBM", "www.ibm.com", "cloud"),
    ("oracle", "Oracle", "www.oracle.com", "cloud"),
    ("digitalocean", "DigitalOcean", "www.digitalocean.com", "cloud"),
    ("linode", "Linode", "www.linode.com", "cloud"),
    ("hetzner", "Hetzner", "www.hetzner.com", "cloud"),
    ("ovhcloud", "OVHcloud", "www.ovhcloud.com", "cloud"),
    ("fastly", "Fastly", "www.fastly.com", "cloud"),
    ("akamai", "Akamai", "www.akamai.com", "cloud"),
    ("salesforce", "Salesforce", "www.salesforce.com", "cloud"),
    ("sap", "SAP", "www.sap.com", "cloud"),
    ("vmware", "VMware", "www.vmware.com", "cloud"),
    ("cisco", "Cisco", "www.cisco.com", "cloud"),
    ("dell", "Dell", "www.dell.com", "cloud"),
    ("hpe", "HPE", "www.hpe.com", "cloud"),
    ("intel", "Intel", "www.intel.com", "cloud"),
    ("nvidia", "NVIDIA", "www.nvidia.com", "cloud"),
    ("apple", "Apple", "www.apple.com", "cloud"),
    ("google", "Google", "www.google.com", "cloud"),
    ("mozilla", "Mozilla", "www.mozilla.org", "cloud"),
    ("redhat", "Red Hat", "www.redhat.com", "developer"),
    ("canonical", "Canonical", "www.canonical.com", "developer"),
    ("docker", "Docker", "www.docker.com", "developer"),
    # social / media
    ("facebook", "Facebook", "www.facebook.com", "social"),
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
    ("medium", "Medium", "www.medium.com", "social"),
    # e-commerce
    ("amazon", "Amazon", "www.amazon.com", "retail"),
    ("ebay", "eBay", "www.ebay.com", "retail"),
    ("walmart", "Walmart", "www.walmart.com", "retail"),
    ("target", "Target", "www.target.com", "retail"),
    ("costco", "Costco", "www.costco.com", "retail"),
    ("bestbuy", "Best Buy", "www.bestbuy.com", "retail"),
    ("homedepot", "Home Depot", "www.homedepot.com", "retail"),
    ("lowes", "Lowe's", "www.lowes.com", "retail"),
    ("macys", "Macy's", "www.macys.com", "retail"),
    ("nordstrom", "Nordstrom", "www.nordstrom.com", "retail"),
    ("shopify", "Shopify", "www.shopify.com", "retail"),
    ("etsy", "Etsy", "www.etsy.com", "retail"),
    ("aliexpress", "AliExpress", "www.aliexpress.com", "retail"),
    ("alibaba", "Alibaba", "www.alibaba.com", "retail"),
    ("taobao", "Taobao", "www.taobao.com", "retail"),
    ("jd", "JD.com", "www.jd.com", "retail"),
    ("rakuten", "Rakuten", "www.rakuten.com", "retail"),
    ("wayfair", "Wayfair", "www.wayfair.com", "retail"),
    ("zara", "Zara", "www.zara.com", "retail"),
    ("uniqlo", "Uniqlo", "www.uniqlo.com", "retail"),
    ("nike", "Nike", "www.nike.com", "retail"),
    ("adidas", "Adidas", "www.adidas.com", "retail"),
    # news / reference content
    ("wikipedia", "Wikipedia", "www.wikipedia.org", "news"),
    ("hn", "Hacker News", "news.ycombinator.com", "news"),
    ("reuters", "Reuters", "www.reuters.com", "news"),
    ("bbc", "BBC", "www.bbc.com", "news"),
    ("cnn", "CNN", "www.cnn.com", "news"),
    ("nytimes", "New York Times", "www.nytimes.com", "news"),
    ("guardian", "The Guardian", "www.theguardian.com", "news"),
    ("washingtonpost", "Washington Post", "www.washingtonpost.com", "news"),
    ("wsj", "Wall Street Journal", "www.wsj.com", "news"),
    ("ft", "Financial Times", "www.ft.com", "news"),
    ("npr", "NPR", "www.npr.org", "news"),
    ("ap", "Associated Press", "www.apnews.com", "news"),
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
    ("imf", "IMF", "www.imf.org", "government"),
    ("oecd", "OECD", "www.oecd.org", "government"),
    ("nist", "NIST", "www.nist.gov", "government"),
    ("nasa", "NASA", "www.nasa.gov", "government"),
    ("noaa", "NOAA", "www.noaa.gov", "government"),
    ("mit", "MIT", "www.mit.edu", "education"),
    ("stanford", "Stanford", "www.stanford.edu", "education"),
    ("harvard", "Harvard", "www.harvard.edu", "education"),
    ("oxford", "Oxford", "www.ox.ac.uk", "education"),
    ("cambridge", "Cambridge", "www.cam.ac.uk", "education"),
    # healthcare
    ("cdc", "CDC", "www.cdc.gov", "health"),
    ("nih", "NIH", "www.nih.gov", "health"),
    ("fda", "FDA", "www.fda.gov", "health"),
    ("mayoclinic", "Mayo Clinic", "www.mayoclinic.org", "health"),
    ("clevelandclinic", "Cleveland Clinic", "www.clevelandclinic.org", "health"),
    ("hopkins", "Johns Hopkins Medicine", "www.hopkinsmedicine.org", "health"),
    ("nhs", "NHS", "www.nhs.uk", "health"),
    ("healthline", "Healthline", "www.healthline.com", "health"),
    ("webmd", "WebMD", "www.webmd.com", "health"),
    # telecom
    ("att", "AT&T", "www.att.com", "telecom"),
    ("verizon", "Verizon", "www.verizon.com", "telecom"),
    ("tmobile", "T-Mobile", "www.t-mobile.com", "telecom"),
    ("comcast", "Comcast", "www.comcast.net", "telecom"),
    ("spectrum", "Spectrum", "www.spectrum.com", "telecom"),
    ("bt", "BT", "www.bt.com", "telecom"),
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
    ("zoom", "Zoom", "www.zoom.us", "developer"),
    ("notion", "Notion", "www.notion.so", "developer"),
    ("figma", "Figma", "www.figma.com", "developer"),
    ("canva", "Canva", "www.canva.com", "developer"),
    ("dropbox", "Dropbox", "www.dropbox.com", "developer"),
    ("openai", "OpenAI", "www.openai.com", "developer"),
    ("anthropic", "Anthropic", "www.anthropic.com", "developer"),
    ("huggingface", "Hugging Face", "www.huggingface.co", "developer"),
    ("npm", "npm", "www.npmjs.com", "developer"),
    ("kubernetes", "Kubernetes", "www.kubernetes.io", "developer"),
    ("nginx", "NGINX", "www.nginx.com", "developer"),
    ("redis", "Redis", "www.redis.io", "developer"),
    ("postgresql", "PostgreSQL", "www.postgresql.org", "developer"),
    ("mysql", "MySQL", "www.mysql.com", "developer"),
    ("python", "Python", "www.python.org", "developer"),
    ("golang", "Go", "www.golang.org", "developer"),
    ("rust", "Rust", "www.rust-lang.org", "developer"),
    # additional public endpoints to bring the service tier to 240
    ("bing", "Bing", "www.bing.com", "news"),
    ("yahoo", "Yahoo", "www.yahoo.com", "news"),
    ("duckduckgo", "DuckDuckGo", "duckduckgo.com", "news"),
    ("brave", "Brave", "www.brave.com", "developer"),
    ("startpage", "Startpage", "www.startpage.com", "news"),
    ("ecosia", "Ecosia", "www.ecosia.org", "news"),
    ("baidu", "Baidu", "www.baidu.com", "news"),
    ("yandex", "Yandex", "yandex.com", "news"),
    ("naver", "Naver", "www.naver.com", "news"),
    ("seznam", "Seznam", "www.seznam.cz", "news"),
    ("mailru", "Mail.ru", "mail.ru", "news"),
    ("zhihu", "Zhihu", "www.zhihu.com", "social"),
    ("bilibili", "Bilibili", "www.bilibili.com", "social"),
    ("tmall", "Tmall", "www.tmall.com", "retail"),
    ("pinduoduo", "Pinduoduo", "www.pinduoduo.com", "retail"),
    ("meituan", "Meituan", "www.meituan.com", "retail"),
    ("ctrip", "Ctrip", "www.ctrip.com", "retail"),
    ("suning", "Suning", "www.suning.com", "retail"),
    ("mi", "Xiaomi", "www.mi.com", "cloud"),
    ("huawei", "Huawei", "www.huawei.com", "cloud"),
    ("oppo", "OPPO", "www.oppo.com", "cloud"),
    ("vivo", "vivo", "www.vivo.com", "cloud"),
    ("oneplus", "OnePlus", "www.oneplus.com", "cloud"),
    ("samsung", "Samsung", "www.samsung.com", "cloud"),
    ("lg", "LG", "www.lg.com", "cloud"),
    ("sony", "Sony", "www.sony.com", "cloud"),
    ("lenovo", "Lenovo", "www.lenovo.com", "cloud"),
    ("asus", "ASUS", "www.asus.com", "cloud"),
    ("acer", "Acer", "www.acer.com", "cloud"),
    ("msi", "MSI", "www.msi.com", "cloud"),
    ("gigabyte", "GIGABYTE", "www.gigabyte.com", "cloud"),
    ("ikea", "IKEA", "www.ikea.com", "retail"),
    ("carrefour", "Carrefour", "www.carrefour.com", "retail"),
    ("tesco", "Tesco", "www.tesco.com", "retail"),
    ("sainsburys", "Sainsbury's", "www.sainsburys.co.uk", "retail"),
    ("mercadolibre", "Mercado Libre", "www.mercadolibre.com", "retail"),
    ("latam", "LATAM", "www.latam.com", "airline"),
    ("airnewzealand", "Air New Zealand", "www.airnewzealand.co.nz", "airline"),
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
    ns = chosen_ns[tld]
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
    w("    class: service")
    w(f"    sector: {y(sector)}")
    if sector in ("financial/banking", "cloud", "developer"):
        w("    tier: 1")
        w("    interval_s: 120")
    else:
        w("    tier: 2")
        w("    interval_s: 300")
    counts["service"] += 1

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
# count: corridors are evidence about a path, never probe targets.
if CORRIDORS:
    w("corridors:")
    for cid, frm, to, near, far in CORRIDORS:
        w(f"  - id: {cid}")
        w(f"    from: {frm}")
        w(f"    to: {to}")
        w(f"    endpoints: [{y(near)}, {y(far)}]")

with open(OUT, "w", encoding="utf-8", newline="\n") as f:
    f.write("\n".join(out) + "\n")

print(f"wrote {OUT}: {dict(counts)} total {sum(counts.values())}")
