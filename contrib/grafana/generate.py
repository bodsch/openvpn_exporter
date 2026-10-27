#!/usr/bin/env python3
"""Generates the Grafana dashboard for openvpn_exporter.

Usage: python3 contrib/grafana/generate.py contrib/grafana/openvpn.json
"""
import json
import sys

DS = {"type": "prometheus", "uid": "${datasource}"}
SRV = 'server=~"$server"'
CLI = 'server=~"$server", common_name=~"$client"'

panels = []
_id = [0]


def nid():
    _id[0] += 1
    return _id[0]


def pos(x, y, w, h):
    return {"x": x, "y": y, "w": w, "h": h}


def target(expr, legend="", ref="A", instant=False, table=False):
    t = {"datasource": DS, "refId": ref, "expr": expr, "legendFormat": legend or "__auto"}
    if instant or table:
        t.update({"instant": True, "range": False})
    if table:
        t["format"] = "table"
    return t


def thresholds(*steps):
    """steps: (color, value) with the first value None."""
    return {"mode": "absolute", "steps": [{"color": c, "value": v} for c, v in steps]}


def row(title, y):
    panels.append({"id": nid(), "type": "row", "title": title, "collapsed": False,
                   "gridPos": pos(0, y, 24, 1), "panels": []})


def stat(title, expr, x, y, w=3, h=4, unit="none", th=None, mappings=None, color_mode="background",
         legend="", text_mode="value", graph="none", decimals=None, description=""):
    defaults = {"unit": unit, "mappings": mappings or [],
                "color": {"mode": "thresholds" if th else "fixed", "fixedColor": "text"},
                "thresholds": th or thresholds(("text", None))}
    if decimals is not None:
        defaults["decimals"] = decimals
    panels.append({
        "id": nid(), "type": "stat", "title": title, "description": description, "datasource": DS,
        "gridPos": pos(x, y, w, h),
        "targets": [target(expr, legend, instant=graph == "none")],
        "fieldConfig": {"defaults": defaults, "overrides": []},
        "options": {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
                    "colorMode": color_mode if th else "none", "graphMode": graph,
                    "textMode": text_mode, "justifyMode": "center", "orientation": "auto",
                    "wideLayout": True, "showPercentChange": False},
    })


def timeseries(title, targets, x, y, w=12, h=8, unit="none", min_=0, max_=None, stack=False,
               fill=0, description="", legend_calcs=("mean", "max", "lastNotNull")):
    custom = {"lineWidth": 2, "fillOpacity": fill, "showPoints": "never", "spanNulls": False,
              "drawStyle": "line", "lineInterpolation": "linear", "axisBorderShow": False,
              "gradientMode": "none", "axisSoftMin": min_,
              "stacking": {"mode": "normal" if stack else "none", "group": "A"}}
    defaults = {"unit": unit, "custom": custom, "color": {"mode": "palette-classic-by-name"}}
    if max_ is not None:
        defaults["max"] = max_
    panels.append({
        "id": nid(), "type": "timeseries", "title": title, "description": description,
        "datasource": DS, "gridPos": pos(x, y, w, h), "targets": targets,
        "fieldConfig": {"defaults": defaults, "overrides": []},
        "options": {"legend": {"displayMode": "table", "placement": "bottom", "showLegend": True,
                               "calcs": list(legend_calcs), "sortBy": "Max", "sortDesc": True},
                    "tooltip": {"mode": "multi", "sort": "desc"}},
    })


def state_timeline(title, expr, legend, mappings, x, y, w=12, h=8, description=""):
    panels.append({
        "id": nid(), "type": "state-timeline", "title": title, "description": description,
        "datasource": DS, "gridPos": pos(x, y, w, h),
        "targets": [target(expr, legend)],
        "fieldConfig": {"defaults": {"mappings": mappings, "color": {"mode": "thresholds"},
                                     "thresholds": OK_TH,
                                     "custom": {"lineWidth": 0, "fillOpacity": 80}},
                        "overrides": []},
        "options": {"showValue": "never", "mergeValues": True, "alignValue": "left",
                    "rowHeight": 0.85, "legend": {"showLegend": False},
                    "tooltip": {"mode": "single", "sort": "none"}},
    })


def table(title, targets, x, y, w, h, rename, order, overrides, exclude=(), sort=None, description=""):
    back = {v: k for k, v in rename.items()}
    excl = {"Time": True, "instance": True, "job": True, "__name__": True}
    excl.update({k: True for k in exclude})
    opts = {"showHeader": True, "cellHeight": "sm", "footer": {"show": False}}
    if sort:
        opts["sortBy"] = [{"displayName": sort[0], "desc": sort[1]}]
    panels.append({
        "id": nid(), "type": "table", "title": title, "description": description, "datasource": DS,
        "gridPos": pos(x, y, w, h), "targets": targets,
        "transformations": [
            {"id": "merge", "options": {}},
            {"id": "organize", "options": {"excludeByName": excl, "renameByName": rename,
                                           "indexByName": {back.get(n, n): i for i, n in enumerate(order)}}},
        ],
        "fieldConfig": {"defaults": {"custom": {"align": "auto", "cellOptions": {"type": "auto"},
                                                "filterable": True, "minWidth": 70},
                                     "mappings": [], "thresholds": thresholds(("text", None))},
                        "overrides": overrides},
        "options": opts,
    })


def override(name, **props):
    return {"matcher": {"id": "byName", "options": name},
            "properties": [{"id": k.replace("__", "."), "value": v} for k, v in props.items()]}


def value_map(*entries):
    """entries: (value, text, color)"""
    return [{"type": "value", "options": {str(v): {"text": t, "color": c, "index": i}
                                          for i, (v, t, c) in enumerate(entries)}}]


NULL_MAP = {"type": "special", "options": {"match": "null", "result": {"text": "–", "color": "transparent", "index": 9}}}
UP_MAP = value_map((1, "UP", "green"), (0, "DOWN", "red"))
PING_MAP = value_map((1, "OK", "green"), (0, "keine Antwort", "red")) + [NULL_MAP]
CONN_MAP = [
    {"type": "value", "options": {"0": {"text": "getrennt", "color": "red", "index": 0}}},
    {"type": "range", "options": {"from": 1, "to": 1e9,
                                  "result": {"text": "verbunden", "color": "green", "index": 1}}},
    NULL_MAP,
]
OK_TH = thresholds(("red", None), ("green", 1))
BG = {"type": "color-background", "mode": "basic"}
DAYS_TH = thresholds(("red", None), ("orange", 7), ("green", 30))

# --- Übersicht -------------------------------------------------------------------------
y = 0
stat("Server", f"min(openvpn_up{{{SRV}}})", 0, y, mappings=UP_MAP,
     th=thresholds(("red", None), ("green", 1)),
     description="Status des Management-Interface bzw. der Status-Datei (schlechtester Server).")
stat("Version", f"openvpn_server_info{{{SRV}}}", 3, y, legend="{{version}}", text_mode="name",
     description="Gemeldete Version aller ausgewählten Server.")
stat("Verbundene Clients", f"sum(openvpn_connected_clients{{{SRV}}})", 6, y, graph="area")
stat("Clients fehlen", f"count(openvpn_client_connections{{{SRV}}} == 0) or vector(0)", 9, y,
     th=thresholds(("green", None), ("red", 1)),
     description="Clients aus expected_clients, die aktuell nicht verbunden sind.")
stat("Unerreichbar", f"count(openvpn_client_ping_success{{{SRV}}} == 0) or vector(0)", 12, y,
     th=thresholds(("green", None), ("red", 1)),
     description="Ping-Ziele ohne Antwort.")
stat("Zertifikat",
     f"min((openvpn_certificate_not_after_timestamp_seconds{{{SRV}}} - time()) / 86400)", 15, y,
     unit="suffix: Tage", decimals=0, th=DAYS_TH,
     description="Tage bis zum Ablauf: kürzeste Restlaufzeit aller überwachten Zertifikate (Server, CA).")
stat("CRL",
     f"min((openvpn_crl_next_update_timestamp_seconds{{{SRV}}} - time()) / 86400)", 18, y,
     unit="suffix: Tage", decimals=1, th=thresholds(("red", None), ("orange", 7), ("green", 14)),
     description="Tage bis zum Ablauf der CRL (nextUpdate). Eine abgelaufene CRL sperrt ALLE Clients aus.")
stat("Status-Alter", f"max(time() - openvpn_status_updated_timestamp_seconds{{{SRV}}})", 21, y,
     unit="s", th=thresholds(("green", None), ("orange", 120), ("red", 300)),
     description="Wie alt der von OpenVPN gemeldete Status ist. Bei Status-Dateien: Datei wird nicht mehr aktualisiert?")

# --- Clients ---------------------------------------------------------------------------
y = 4
row("Clients", y)
y += 1
agg = "max by (server, common_name)"
table(
    "Clients",
    [
        target(f"{agg} (openvpn_client_connections{{{CLI}}})", ref="A", table=True),
        target(f"time() - {agg} (openvpn_client_connected_since_timestamp_seconds{{{CLI}}})", ref="B", table=True),
        target(f"time() - {agg} (openvpn_client_last_activity_timestamp_seconds{{{CLI}}})", ref="C", table=True),
        target(f"{agg} (openvpn_client_ping_success{{{CLI}}})", ref="D", table=True),
        target(f"{agg} (openvpn_client_ping_rtt_seconds{{{CLI}}})", ref="E", table=True),
        target(f"sum by (server, common_name) (rate(openvpn_client_received_bytes_total{{{CLI}}}[$__rate_interval])) * 8", ref="F", table=True),
        target(f"sum by (server, common_name) (rate(openvpn_client_sent_bytes_total{{{CLI}}}[$__rate_interval])) * 8", ref="G", table=True),
        target(f"max by (server, common_name, real_address, virtual_address, cipher) (openvpn_client_info{{{CLI}}})", ref="H", table=True),
    ],
    0, y, 24, 8,
    rename={"common_name": "Client", "server": "Server", "real_address": "Remote-IP",
            "virtual_address": "VPN-IP", "cipher": "Cipher",
            "Value #A": "Status", "Value #B": "Online seit", "Value #C": "Aktiv vor",
            "Value #D": "Ping", "Value #E": "RTT", "Value #F": "Empfangen", "Value #G": "Gesendet"},
    order=["Server", "Client", "Status", "Remote-IP", "VPN-IP", "Online seit", "Aktiv vor",
           "Ping", "RTT", "Empfangen", "Gesendet", "Cipher"],
    exclude=["Value #H"],
    sort=("Client", False),
    overrides=[
        override("Status", mappings=CONN_MAP, thresholds=OK_TH, custom__cellOptions=BG, custom__width=110),
        override("Ping", mappings=PING_MAP, thresholds=OK_TH, custom__cellOptions=BG, custom__width=120),
        override("Online seit", unit="dtdurations"),
        override("Aktiv vor", unit="s", decimals=0),
        override("RTT", unit="s"),
        override("Empfangen", unit="bps"),
        override("Gesendet", unit="bps"),
    ],
    description="Ein Eintrag pro Common Name. Fehlende erwartete Clients erscheinen mit Status 'getrennt'.",
)
y += 8
timeseries("Verbundene Clients", [target(f"sum by (server) (openvpn_connected_clients{{{SRV}}})", "{{server}}")],
           0, y, unit="none", legend_calcs=("min", "max", "lastNotNull"))
state_timeline("Verbindungsstatus", f"{agg} (openvpn_client_connections{{{CLI}}})", "{{common_name}}",
               CONN_MAP, 12, y, description="Zeigt Verbindungsabbrüche und Flapping pro Client.")
y += 8

# --- Durchsatz -------------------------------------------------------------------------
row("Durchsatz", y)
y += 1
timeseries("Empfangen vom Client",
           [target(f"sum by (common_name) (rate(openvpn_client_received_bytes_total{{{CLI}}}[$__rate_interval])) * 8",
                   "{{common_name}}")], 0, y, unit="bps")
timeseries("Gesendet an Client",
           [target(f"sum by (common_name) (rate(openvpn_client_sent_bytes_total{{{CLI}}}[$__rate_interval])) * 8",
                   "{{common_name}}")], 12, y, unit="bps")
y += 8
timeseries("Server gesamt",
           [target(f"sum by (server) (rate(openvpn_server_received_bytes_total{{{SRV}}}[$__rate_interval])) * 8",
                   "empfangen {{server}}", ref="A"),
            target(f"sum by (server) (rate(openvpn_server_sent_bytes_total{{{SRV}}}[$__rate_interval])) * 8",
                   "gesendet {{server}}", ref="B")],
           0, y, unit="bps",
           description="Gesamtverkehr aller Links laut load-stats (nur mit Management-Interface).")
panels.append({
    "id": nid(), "type": "bargauge", "title": "Datenvolumen im Zeitraum",
    "description": "Empfangen + gesendet pro Client im gewählten Zeitraum.",
    "datasource": DS, "gridPos": pos(12, y, 12, 8),
    "targets": [target(
        f"sort_desc(sum by (common_name) (increase(openvpn_client_received_bytes_total{{{CLI}}}[$__range])"
        f" + increase(openvpn_client_sent_bytes_total{{{CLI}}}[$__range])))",
        "{{common_name}}", instant=True)],
    "fieldConfig": {"defaults": {"unit": "decbytes", "min": 0, "color": {"mode": "fixed", "fixedColor": "blue"},
                                 "thresholds": thresholds(("blue", None))}, "overrides": []},
    "options": {"orientation": "horizontal", "displayMode": "basic", "showUnfilled": True,
                "valueMode": "text", "namePlacement": "left", "sizing": "auto", "minVizHeight": 16,
                "maxVizHeight": 24,
                "reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False}},
})
y += 8

# --- Erreichbarkeit --------------------------------------------------------------------
row("Erreichbarkeit", y)
y += 1
state_timeline("Ping-Status", f"max by (common_name, address) (openvpn_client_ping_success{{{CLI}}})",
               "{{common_name}}", PING_MAP, 0, y, w=24, h=7,
               description="Ein Balken pro Ping-Ziel; die Adresse steht in der Clients-Tabelle und im Tooltip der RTT.")
y += 7
timeseries("Antwortzeit (RTT)",
           [target(f"max by (common_name, address) (openvpn_client_ping_rtt_seconds{{{CLI}}})",
                   "{{common_name}} ({{address}})")], 0, y, unit="s")
timeseries("Paketverlust",
           [target(f"max by (common_name, address) (openvpn_client_ping_packet_loss_ratio{{{CLI}}})",
                   "{{common_name}} ({{address}})")], 12, y, unit="percentunit", max_=1)
y += 8

# --- Zertifikate -----------------------------------------------------------------------
row("Zertifikate & CRL", y)
y += 1
cert_labels = "server, file, subject, issuer, serial"
table(
    "Zertifikate",
    [target(f"max by ({cert_labels}) ((openvpn_certificate_not_after_timestamp_seconds{{{SRV}}} - time()) / 86400)",
            ref="A", table=True),
     target(f"max by ({cert_labels}) (openvpn_certificate_not_after_timestamp_seconds{{{SRV}}}) * 1000",
            ref="B", table=True)],
    0, y, 24, 6,
    rename={"server": "Server", "subject": "Subject", "issuer": "Aussteller", "serial": "Seriennummer",
            "file": "Datei", "Value #A": "Restlaufzeit", "Value #B": "Gültig bis"},
    order=["Server", "Subject", "Restlaufzeit", "Gültig bis", "Aussteller", "Seriennummer", "Datei"],
    sort=("Restlaufzeit", False),
    overrides=[
        override("Restlaufzeit", unit="suffix: Tage", decimals=0, thresholds=DAYS_TH,
                 custom__cellOptions=BG, custom__width=130),
        override("Gültig bis", unit="dateTimeAsIso"),
    ],
)
y += 6
crl_labels = "server, file, issuer"
table(
    "Certificate Revocation Lists",
    [target(f"max by ({crl_labels}) ((openvpn_crl_next_update_timestamp_seconds{{{SRV}}} - time()) / 86400)",
            ref="A", table=True),
     target(f"max by ({crl_labels}) (openvpn_crl_next_update_timestamp_seconds{{{SRV}}}) * 1000",
            ref="B", table=True),
     target(f"max by ({crl_labels}) (openvpn_crl_revoked_certificates{{{SRV}}})", ref="C", table=True)],
    0, y, 24, 5,
    rename={"server": "Server", "issuer": "Aussteller", "file": "Datei",
            "Value #A": "Restlaufzeit", "Value #B": "nextUpdate", "Value #C": "Gesperrte Zertifikate"},
    order=["Server", "Aussteller", "Restlaufzeit", "nextUpdate", "Gesperrte Zertifikate", "Datei"],
    overrides=[
        override("Restlaufzeit", unit="suffix: Tage", decimals=1,
                 thresholds=thresholds(("red", None), ("orange", 7), ("green", 14)),
                 custom__cellOptions=BG, custom__width=130),
        override("nextUpdate", unit="dateTimeAsIso"),
    ],
    description="Eine abgelaufene CRL sperrt bei crl-verify ALLE Clients aus. Erneuern mit 'easyrsa gen-crl'.",
)


def var_query(name, label, query, include_all=True):
    return {"type": "query", "name": name, "label": label, "datasource": DS,
            "query": {"query": query, "refId": f"var-{name}", "qryType": 1},
            "definition": query, "refresh": 2, "multi": True, "includeAll": include_all,
            "allValue": ".*", "current": {"text": ["All"], "value": ["$__all"]}, "sort": 1}


dashboard = {
    "uid": "openvpn-exporter",
    "title": "OpenVPN",
    "description": "Server, Clients, Durchsatz, Erreichbarkeit und Zertifikate (openvpn_exporter).",
    "tags": ["openvpn", "vpn"],
    "editable": True,
    "graphTooltip": 1,
    "timezone": "browser",
    "time": {"from": "now-6h", "to": "now"},
    "refresh": "1m",
    "schemaVersion": 39,
    "version": 1,
    "links": [],
    "annotations": {"list": [{"builtIn": 1, "datasource": {"type": "grafana", "uid": "-- Grafana --"},
                              "enable": True, "hide": True, "iconColor": "rgba(0, 211, 255, 1)",
                              "name": "Annotations & Alerts", "type": "dashboard"}]},
    "templating": {"list": [
        {"type": "datasource", "name": "datasource", "label": "Datenquelle", "query": "prometheus",
         "current": {}, "hide": 0, "refresh": 1, "regex": ""},
        var_query("server", "Server", "label_values(openvpn_up, server)"),
        var_query("client", "Client", 'label_values(openvpn_client_connections{server=~"$server"}, common_name)'),
    ]},
    "panels": panels,
}

json.dump(dashboard, open(sys.argv[1], "w"), indent=2, ensure_ascii=False)
open(sys.argv[1], "a").write("\n")
print(f"{len(panels)} panels")
