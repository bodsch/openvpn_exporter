// Package openvpn reads OpenVPN server status, either from the management
// interface or from a status file.
package openvpn

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Status is the parsed output of "status" (versions 1, 2 and 3).
type Status struct {
	Title       string
	Version     string
	UpdatedAt   time.Time
	Clients     []Client
	Routes      []Route
	GlobalStats map[string]float64
}

// Client is one entry of the CLIENT_LIST section.
type Client struct {
	CommonName         string
	RealAddress        string
	VirtualAddress     string
	VirtualIPv6Address string
	Username           string
	Cipher             string
	BytesReceived      uint64
	BytesSent          uint64
	ConnectedSince     time.Time
}

// Route is one entry of the ROUTING_TABLE section.
type Route struct {
	VirtualAddress string
	CommonName     string
	RealAddress    string
	LastRef        time.Time
}

const (
	colCommonName      = "Common Name"
	colRealAddress     = "Real Address"
	colVirtualAddress  = "Virtual Address"
	colVirtualIPv6     = "Virtual IPv6 Address"
	colBytesReceived   = "Bytes Received"
	colBytesSent       = "Bytes Sent"
	colConnectedSince  = "Connected Since"
	colConnectedSinceT = "Connected Since (time_t)"
	colUsername        = "Username"
	colCipher          = "Data Channel Cipher"
	colLastRef         = "Last Ref"
	colLastRefT        = "Last Ref (time_t)"
)

// Time formats used by OpenVPN for human readable timestamps.
var timeLayouts = []string{
	"2006-01-02 15:04:05",      // OpenVPN >= 2.5
	"Mon Jan _2 15:04:05 2006", // OpenVPN < 2.5
}

var versionRe = regexp.MustCompile(`^OpenVPN (\S+)`)

// row gives access to the fields of a data line by column name.
type row struct {
	cols   map[string]int
	fields []string
}

func (r row) get(name string) string {
	i, ok := r.cols[name]
	if !ok || i >= len(r.fields) {
		return ""
	}
	return r.fields[i]
}

func (r row) time(textCol, unixCol string) (time.Time, error) {
	if v := r.get(unixCol); v != "" {
		return parseUnix(v)
	}
	if v := r.get(textCol); v != "" {
		return parseTime(v)
	}
	return time.Time{}, nil
}

func parseUnix(v string) (time.Time, error) {
	sec, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid unix timestamp %q", v)
	}
	return time.Unix(sec, 0), nil
}

func parseTime(v string) (time.Time, error) {
	for _, layout := range timeLayouts {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unknown time format %q", v)
}

// ParseStatus parses OpenVPN status output in version 1, 2 or 3 format.
func ParseStatus(r io.Reader) (*Status, error) {
	var lines []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, strings.TrimRight(sc.Text(), "\r"))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return parseLines(lines)
}

func parseLines(lines []string) (*Status, error) {
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("empty status")
	}

	var (
		s   *Status
		err error
	)
	switch first := lines[0]; {
	case strings.HasPrefix(first, "TITLE\t"):
		s, err = parseV2V3(lines, "\t")
	case strings.HasPrefix(first, "TITLE,"):
		s, err = parseV2V3(lines, ",")
	case first == "OpenVPN CLIENT LIST":
		s, err = parseV1(lines)
	case first == "OpenVPN STATISTICS":
		return nil, fmt.Errorf("status is from an OpenVPN client or point-to-point instance, only server mode is supported")
	default:
		return nil, fmt.Errorf("unknown status format, first line: %q", first)
	}
	if err != nil {
		return nil, err
	}
	if m := versionRe.FindStringSubmatch(s.Title); m != nil {
		s.Version = m[1]
	}
	return s, nil
}

// parseV2V3 parses "status 2" (comma separated) and "status 3" (tab separated).
func parseV2V3(lines []string, sep string) (*Status, error) {
	s := &Status{GlobalStats: map[string]float64{}}
	headers := map[string]map[string]int{}

	for n, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.Split(line, sep)
		switch fields[0] {
		case "TITLE":
			if len(fields) > 1 {
				s.Title = fields[1]
			}
		case "TIME":
			var err error
			switch {
			case len(fields) > 2:
				s.UpdatedAt, err = parseUnix(fields[2])
			case len(fields) > 1:
				s.UpdatedAt, err = parseTime(fields[1])
			}
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", n+1, err)
			}
		case "HEADER":
			if len(fields) < 2 {
				continue
			}
			cols := map[string]int{}
			for i, name := range fields[2:] {
				cols[name] = i + 1
			}
			headers[fields[1]] = cols
		case "CLIENT_LIST":
			c, err := parseClient(row{cols: headerOr(headers, "CLIENT_LIST", defaultClientCols), fields: fields})
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", n+1, err)
			}
			s.Clients = append(s.Clients, c)
		case "ROUTING_TABLE":
			rt, err := parseRoute(row{cols: headerOr(headers, "ROUTING_TABLE", defaultRouteCols), fields: fields})
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", n+1, err)
			}
			s.Routes = append(s.Routes, rt)
		case "GLOBAL_STATS":
			if len(fields) > 2 {
				if v, err := strconv.ParseFloat(fields[2], 64); err == nil {
					s.GlobalStats[fields[1]] = v
				}
			}
		case "END":
			return s, nil
		}
	}
	return s, nil
}

// Column layout of OpenVPN 2.4+ for the unlikely case of a missing HEADER line.
var (
	defaultClientCols = map[string]int{
		colCommonName: 1, colRealAddress: 2, colVirtualAddress: 3, colVirtualIPv6: 4,
		colBytesReceived: 5, colBytesSent: 6, colConnectedSince: 7, colConnectedSinceT: 8,
		colUsername: 9,
	}
	defaultRouteCols = map[string]int{
		colVirtualAddress: 1, colCommonName: 2, colRealAddress: 3, colLastRef: 4, colLastRefT: 5,
	}
)

func headerOr(headers map[string]map[string]int, name string, def map[string]int) map[string]int {
	if h, ok := headers[name]; ok {
		return h
	}
	return def
}

// parseV1 parses the default "status" / status-version 1 format.
func parseV1(lines []string) (*Status, error) {
	s := &Status{GlobalStats: map[string]float64{}}

	var (
		section string
		cols    map[string]int
	)
	for n, line := range lines {
		if line == "" {
			continue
		}
		switch line {
		case "OpenVPN CLIENT LIST", "ROUTING TABLE", "GLOBAL STATS":
			section, cols = line, nil
			continue
		case "END":
			return s, nil
		}
		fields := strings.Split(line, ",")

		if section == "OpenVPN CLIENT LIST" && fields[0] == "Updated" && len(fields) > 1 {
			t, err := parseTime(fields[1])
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", n+1, err)
			}
			s.UpdatedAt = t
			continue
		}
		if section == "GLOBAL STATS" {
			if len(fields) > 1 {
				if v, err := strconv.ParseFloat(fields[1], 64); err == nil {
					s.GlobalStats[fields[0]] = v
				}
			}
			continue
		}
		if cols == nil {
			cols = map[string]int{}
			for i, name := range fields {
				cols[name] = i
			}
			continue
		}

		r := row{cols: cols, fields: fields}
		switch section {
		case "OpenVPN CLIENT LIST":
			c, err := parseClient(r)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", n+1, err)
			}
			s.Clients = append(s.Clients, c)
		case "ROUTING TABLE":
			rt, err := parseRoute(r)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", n+1, err)
			}
			s.Routes = append(s.Routes, rt)
		}
	}
	return s, nil
}

func parseClient(r row) (Client, error) {
	c := Client{
		CommonName:         r.get(colCommonName),
		RealAddress:        r.get(colRealAddress),
		VirtualAddress:     r.get(colVirtualAddress),
		VirtualIPv6Address: r.get(colVirtualIPv6),
		Username:           r.get(colUsername),
		Cipher:             r.get(colCipher),
	}
	if c.Username == "UNDEF" {
		c.Username = ""
	}
	var err error
	if c.BytesReceived, err = parseUint(r.get(colBytesReceived)); err != nil {
		return c, fmt.Errorf("client %q: bytes received: %w", c.CommonName, err)
	}
	if c.BytesSent, err = parseUint(r.get(colBytesSent)); err != nil {
		return c, fmt.Errorf("client %q: bytes sent: %w", c.CommonName, err)
	}
	if c.ConnectedSince, err = r.time(colConnectedSince, colConnectedSinceT); err != nil {
		return c, fmt.Errorf("client %q: %w", c.CommonName, err)
	}
	return c, nil
}

func parseRoute(r row) (Route, error) {
	rt := Route{
		VirtualAddress: r.get(colVirtualAddress),
		CommonName:     r.get(colCommonName),
		RealAddress:    r.get(colRealAddress),
	}
	var err error
	if rt.LastRef, err = r.time(colLastRef, colLastRefT); err != nil {
		return rt, fmt.Errorf("route %q: %w", rt.VirtualAddress, err)
	}
	return rt, nil
}

func parseUint(v string) (uint64, error) {
	if v == "" {
		return 0, nil
	}
	return strconv.ParseUint(v, 10, 64)
}

// LoadStats is the output of the "load-stats" management command.
type LoadStats struct {
	Clients  int
	BytesIn  uint64
	BytesOut uint64
}

// parseLoadStats parses "nclients=1,bytesin=123,bytesout=456".
func parseLoadStats(v string) (*LoadStats, error) {
	ls := &LoadStats{}
	for _, kv := range strings.Split(v, ",") {
		key, val, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			continue
		}
		var err error
		switch key {
		case "nclients":
			ls.Clients, err = strconv.Atoi(val)
		case "bytesin":
			ls.BytesIn, err = strconv.ParseUint(val, 10, 64)
		case "bytesout":
			ls.BytesOut, err = strconv.ParseUint(val, 10, 64)
		}
		if err != nil {
			return nil, fmt.Errorf("load-stats %s: %w", key, err)
		}
	}
	return ls, nil
}
