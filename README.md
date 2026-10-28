# openvpn_exporter

Prometheus-Exporter für OpenVPN-Server – Nachfolger von `check_openvpn.pl`.
Ein einzelnes Binary deckt ab:

- **verbundene Clients** – Anzahl, pro Common Name, erwartete-aber-fehlende Clients
- **Durchsatz** – Bytes pro Client und Server gesamt
- **Zertifikatslaufzeiten** – Server-, CA-Zertifikat und CRL (`nextUpdate`)
- **Erreichbarkeit** – ICMP-Ping der Clients durch den Tunnel

Datenquelle ist das **Management-Interface** (wie beim Perl-Script) oder alternativ die **Status-Datei**.
Unterstützt werden die Status-Formate 1, 2 und 3 sowie OpenVPN 2.4 bis 2.7.

## Bauen

```sh
make            # lint, test, build
make build      # nur ./openvpn_exporter
```

## OpenVPN vorbereiten

Management-Interface (empfohlen, liefert zusätzlich `load-stats`):

```
# /etc/openvpn/server/server.conf
management 127.0.0.1 7505 /etc/openvpn/server/mgmt.pass
# oder als Unix-Socket:
# management /run/openvpn/server.sock unix
```

OpenVPN erlaubt **nur eine** Management-Verbindung gleichzeitig. Der Exporter verbindet sich pro
Scrape kurz und trennt sofort wieder; Abfragen pro Server werden serialisiert.

Alternativ die Status-Datei:

```
status /run/openvpn/status.log 10
status-version 3
```

## Konfiguration

Siehe [`config.example.yml`](config.example.yml). Minimal:

```yaml
servers:
  - name: vpn
    management:
      address: 127.0.0.1:7505
      password_file: /etc/openvpn/server/mgmt.pass
    certificates: [/etc/openvpn/server/server.crt, /etc/openvpn/server/ca.crt]
    crls: [/etc/openvpn/server/crl.pem]
    expected_clients: [filiale-01, filiale-02]
    ping:
      connected_clients: true
```

```sh
openvpn_exporter --config.file=/etc/openvpn_exporter/config.yml --config.check
openvpn_exporter --config.file=/etc/openvpn_exporter/config.yml   # :9176/metrics
```

| Flag | Default |
|---|---|
| `--config.file` | `/etc/openvpn_exporter/config.yml` |
| `--config.check` | Konfiguration prüfen und beenden |
| `--web.listen-address` | `:9176` |
| `--web.telemetry-path` | `/metrics` |
| `--web.config.file` | leer (kein TLS, keine Auth) |
| `--web.max-requests` | `3` (parallele Scrapes, `0` = unbegrenzt) |
| `--log.level` | `info` |

### TLS und Basic Auth

Die Metriken enthalten Common Names, Client-IPs und Usernames. Ist der Port
von außen erreichbar, sollte `--web.config.file` gesetzt werden. Das Format ist
das der [Prometheus exporter-toolkit](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md):

```yaml
# /etc/openvpn_exporter/web.yml
tls_server_config:
  cert_file: /etc/openvpn_exporter/tls.crt
  key_file: /etc/openvpn_exporter/tls.key
basic_auth_users:
  prometheus: $2y$10$...   # bcrypt, z. B. htpasswd -nBC 10 "" | tr -d ':\n'
```

`--config.check` prüft die Web-Konfiguration mit.

### Ping

Der Exporter muss auf dem VPN-Server laufen, damit die Tunnel-Routen existieren.
Standardmäßig werden unprivilegierte ICMP-Sockets genutzt; dafür muss die Gruppe des Exporters in
`net.ipv4.ping_group_range` liegen (bei systemd-basierten Distributionen meist Default).
Sonst `privileged: true` und `CAP_NET_RAW` setzen (siehe `contrib/openvpn_exporter.service`).

Die Pings laufen im Hintergrund alle `check_interval` (Default 30s); ein Scrape liefert nur die letzten
Ergebnisse und wartet nie auf ICMP-Timeouts. Neu verbundene Clients werden sofort angepingt – bis zum
ersten Ergebnis fehlen für sie die `openvpn_client_ping_*`-Metriken.

Ping-Ziele sind die virtuellen IPs aller verbundenen Clients (`connected_clients: true`) und/oder
feste `targets` – z. B. ein Host im LAN hinter einem Site-to-Site-Client. Die Client-Firewall muss
ICMP aus dem Tunnel erlauben.

## Metriken

Alle Metriken tragen das Label `server`.

| Metrik | Labels | Beschreibung |
|---|---|---|
| `openvpn_up` | | Status lesbar (1/0) |
| `openvpn_server_info` | `version` | OpenVPN-Version |
| `openvpn_status_updated_timestamp_seconds` | | Zeitpunkt des Status (Status-Datei veraltet?) |
| `openvpn_status_parse_errors` | | Übersprungene, nicht lesbare Zeilen des letzten Status |
| `openvpn_connected_clients` | | Anzahl verbundener Clients |
| `openvpn_server_received_bytes_total` / `_sent_bytes_total` | | Gesamtverkehr (nur Management) |
| `openvpn_max_bcast_mcast_queue_length`, `openvpn_dco_enabled` | | Globale Statistiken |
| `openvpn_client_connections` | `common_name` | Aktive Verbindungen; **0** für fehlende `expected_clients` |
| `openvpn_client_info` | `common_name`, `real_address`, `virtual_address`, `virtual_ipv6_address`, `username`, `cipher` | Verbindungsdetails |
| `openvpn_client_received_bytes_total` / `_sent_bytes_total` | `common_name` | Bytes vom/zum Client |
| `openvpn_client_connected_since_timestamp_seconds` | `common_name` | Beginn der jüngsten Verbindung |
| `openvpn_client_last_activity_timestamp_seconds` | `common_name` | Letzte Nutzung einer Route des Clients |
| `openvpn_certificate_not_after_timestamp_seconds` / `_not_before_` | `file`, `subject`, `issuer`, `serial` | Gültigkeit der Zertifikate |
| `openvpn_crl_next_update_timestamp_seconds` / `_this_update_` | `file`, `issuer` | Gültigkeit der CRL |
| `openvpn_crl_revoked_certificates` | `file`, `issuer` | Anzahl gesperrter Zertifikate |
| `openvpn_tls_file_error` | `file`, `kind` | Zertifikat/CRL nicht lesbar (1/0) |
| `openvpn_client_ping_success` | `common_name`, `address` | Ping beantwortet (1/0) |
| `openvpn_client_ping_rtt_seconds` | `common_name`, `address` | Mittlere RTT |
| `openvpn_client_ping_packet_loss_ratio` | `common_name`, `address` | Paketverlust 0..1 |

`real_address` enthält bewusst keinen Port, damit nicht bei jedem Reconnect eine neue Zeitreihe entsteht.

### Abfragen

```promql
# Tage bis Ablauf des Server-Zertifikats
(openvpn_certificate_not_after_timestamp_seconds{subject="vpn.example.com"} - time()) / 86400

# Durchsatz pro Client in bit/s
rate(openvpn_client_received_bytes_total[5m]) * 8
rate(openvpn_client_sent_bytes_total[5m]) * 8

# Client von bestimmter IP verbunden? (entspricht check_openvpn.pl -r)
openvpn_client_info{real_address="203.0.113.10"}
```

Fertige Alert-Regeln: [`contrib/alerts.yml`](contrib/alerts.yml).

## Grafana-Dashboard

[`contrib/grafana/openvpn.json`](contrib/grafana/openvpn.json) – in Grafana über
*Dashboards → New → Import* einlesen oder per Provisioning bereitstellen. Getestet mit Grafana 12.

- **Übersicht:** Serverstatus, Version, verbundene/fehlende/unerreichbare Clients,
  Restlaufzeit von Zertifikat und CRL, Alter des Status
- **Clients:** Tabelle pro Client (Status, IPs, Online-Dauer, Ping, RTT, Durchsatz, Cipher),
  Verlauf der Verbindungen und Verbindungsstatus als Zeitleiste
- **Durchsatz:** pro Client in beide Richtungen, Server gesamt, Datenvolumen im Zeitraum
- **Erreichbarkeit:** Ping-Status als Zeitleiste, RTT, Paketverlust
- **Zertifikate & CRL:** Restlaufzeit, Ablaufdatum, Anzahl gesperrter Zertifikate

Variablen: `datasource`, `server`, `client` (Mehrfachauswahl).

Das JSON wird von [`contrib/grafana/generate.py`](contrib/grafana/generate.py) erzeugt.
Änderungen am besten dort vornehmen und neu generieren:

```sh
python3 contrib/grafana/generate.py contrib/grafana/openvpn.json
```

## Vergleich mit `check_openvpn.pl`

| check_openvpn.pl | openvpn_exporter |
|---|---|
| `-n` Anzahl Clients | `openvpn_connected_clients` |
| `-C <cn> -c/-w` | `expected_clients` + Alert `openvpn_client_connections == 0` |
| `-r <ip>` | `openvpn_client_info{real_address="<ip>"}` |
| `-i` IPs auflisten | Label `real_address` von `openvpn_client_info` |
| – | Zertifikate, CRL, Ping, Durchsatz |
