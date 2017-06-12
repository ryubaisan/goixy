package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"os/user"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mitnk/goutils/encrypt"
	"github.com/orcaman/concurrent-map"
)

type GoixyConfig struct {
	Host       string
	Port       string
	Key        string
	WhiteList  []string
	DirectHost string
	DirectPort string
	DirectKey  string
}

var gconfig GoixyConfig = GoixyConfig{}

var VERSION = "1.6.1"
var KEY = []byte("")
var DIRECT_KEY = []byte("")
var countConnected = 0
var DEBUG = false
var VERBOSE = false

// map: server -> bytes received
var Servers = cmap.New()

func main() {
	host := flag.String("host", "127.0.0.1", "host")
	port := flag.String("port", "1080", "port")
	debug := flag.Bool("v", false, "verbose")
	verbose := flag.Bool("vv", false, "very verbose")
	flag.Usage = func() {
		fmt.Printf("Usage of goixy v%s\n", VERSION)
		fmt.Printf("goixy [flags]\n")
		flag.PrintDefaults()
		os.Exit(0)
	}
	flag.Parse()
	DEBUG = *debug
	VERBOSE = *verbose
	loadRouterConfig()

	local, err := net.Listen("tcp", *host+":"+*port)
	if err != nil {
		fmt.Printf("net listen: %v\r", err)
		os.Exit(2)
	}
	defer local.Close()

	info("goixy v%s", VERSION)
	info("listen on port: %s:%s", *host, *port)

	go printServersInfo()
	for {
		client, err := local.Accept()
		if err != nil {
			continue
		}
		go handleClient(client)
	}
}

func printServersInfo() {
	for {
		select {
		case <-time.After(600 * time.Second):
			ts_now := time.Now().Unix()
			keys := Servers.Keys()
			info("[REPORT] We have %d servers connected", len(keys))
			for i, key := range keys {
				if tmp, ok := Servers.Get(key); ok {
					bytes := int64(0)
					ts_span := int64(0)
					if tmp, ok := tmp.(cmap.ConcurrentMap).Get("bytes"); ok {
						bytes = tmp.(int64)
					}
					if tmp, ok := tmp.(cmap.ConcurrentMap).Get("ts"); ok {
						ts_span = ts_now - tmp.(int64)
					}

					str_bytes := ""
					if bytes > 1024*1024*1024 {
						str_bytes += fmt.Sprintf("%.2fG", float64(bytes/(1024.0*1024.0*1024)))
					} else if bytes > 1024*1024 {
						str_bytes += fmt.Sprintf("%.2fM", float64(bytes/(1024.0*1024.0)))
					} else {
						str_bytes += fmt.Sprintf("%.2fK", float64(bytes*1.0/1024.0))
					}

					str_span := ""
					if ts_span > 3600 {
						str_span += fmt.Sprintf("%dh", ts_span/3600)
					}
					if ts_span > 60 {
						str_span += fmt.Sprintf("%dm", (ts_span%3600)/60)
					}
					str_span += fmt.Sprintf("%ds", ts_span%60)
					info("[REPORT] [%d][%s] %s: %s", i, str_span, key, str_bytes)
				}
			}
		}
	}
}

func handleClient(client net.Conn) {
	countConnected += 1
	defer func() {
		client.Close()
		countConnected -= 1
		debug("closed client")
	}()
	info("connected from %v.", client.RemoteAddr())

	data := make([]byte, 1)
	n, err := client.Read(data)
	if err != nil || n != 1 {
		info("cannot read init data from client")
		return
	}
	if data[0] == 5 {
		verbose("handle with socks v5")
		handleSocks(client)
	} else if data[0] > 5 {
		verbose("handle with http")
		handleHTTP(client, data[0])
	} else {
		info("Error: only support HTTP and Socksv5")
	}
}

func handleSocks(client net.Conn) {
	buffer := make([]byte, 1)
	_, err := io.ReadFull(client, buffer)
	if err != nil {
		info("cannot read from client")
		return
	}
	buffer = make([]byte, buffer[0])
	_, err = io.ReadFull(client, buffer)
	if err != nil {
		info("cannot read from client")
		return
	}
	if !byteInArray(0, buffer) {
		info("client not support bare connect")
		return
	}

	// send initial SOCKS5 response (VER, METHOD)
	client.Write([]byte{5, 0})

	buffer = make([]byte, 4)
	_, err = io.ReadFull(client, buffer)
	if err != nil {
		info("failed to read (ver, cmd, rsv, atyp) from client")
		return
	}
	ver, cmd, atyp := buffer[0], buffer[1], buffer[3]
	if ver != 5 {
		info("ver should be 5, got %v", ver)
		return
	}
	// 1: connect 2: bind
	if cmd != 1 && cmd != 2 {
		info("bad cmd:%v", cmd)
		return
	}
	shost := ""
	sport := ""
	if atyp == ATYP_IPV6 {
		info("do not support ipv6 yet")
		return
	} else if atyp == ATYP_DOMAIN {
		buffer = make([]byte, 1)
		_, err = io.ReadFull(client, buffer)
		if err != nil {
			info("cannot read from client")
			return
		}
		buffer = make([]byte, buffer[0])
		_, err = io.ReadFull(client, buffer)
		if err != nil {
			info("cannot read from client")
			return
		}
		shost = string(buffer)
	} else if atyp == ATYP_IPV4 {
		buffer = make([]byte, 4)
		_, err = io.ReadFull(client, buffer)
		if err != nil {
			info("cannot read from client")
			return
		}
		shost = net.IP(buffer).String()
	} else {
		info("bad atyp: %v", atyp)
		return
	}

	buffer = make([]byte, 2)
	_, err = io.ReadFull(client, buffer)
	if err != nil {
		info("cannot read port from client")
		return
	}
	sport = fmt.Sprintf("%d", binary.BigEndian.Uint16(buffer))
	info("server %s:%s", shost, sport)

	// reply to client to estanblish the socks v5 connection
	client.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	rhost, rport, key := getRemoteInfo(shost)
	handleRemote(client, shost, sport, rhost, rport, nil, nil, key)
}

func handleHTTP(client net.Conn, firstByte byte) {
	dataInit := make([]byte, 8192)
	dataInit[0] = firstByte
	nDataInit, err := client.Read(dataInit[1:])
	nDataInit = nDataInit + 1 // plus firstByte
	if err != nil {
		info("cannot read init data from client.")
		return
	}
	isForHTTPS := strings.HasPrefix(string(dataInit[:nDataInit]), "CONNECT")
	verbose("isForHTTPS: %v", isForHTTPS)
	verbose("got content from client:\n%s", dataInit[:nDataInit])

	endor := " HTTP/"
	re := regexp.MustCompile(" .*" + endor)
	s := re.FindString(string(dataInit[:nDataInit]))
	if s == "" {
		// no url found. not valid http proxy protocol?
		return
	}

	s = s[1 : len(s)-len(endor)]
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		info("bad url: %s", s)
		return
	}
	sport := ""
	shost := ""
	host_, port_, _ := net.SplitHostPort(u.Host)
	if port_ != "" {
		sport = port_
		shost = host_
	} else {
		sport = "80"
		shost = u.Host
	}
	info("server %s:%s", shost, sport)
	rhost, rport, key := getRemoteInfo(shost)

	var d2c []byte
	var d2r []byte
	if isForHTTPS {
		d2c = []byte("HTTP/1.0 200 OK\r\n\r\n")
	} else {
		// dataInit := encrypt.Encrypt(dataInit[:nDataInit], key)
		reg1, _ := regexp.Compile("^HEAD https?:..[^/]+/")
		path := reg1.ReplaceAllString(string(dataInit[:nDataInit]), "HEAD /")
		reg2, _ := regexp.Compile("^GET https?:..[^/]+/")
		path = reg2.ReplaceAllString(string(path), "GET /")
		dataInit := encrypt.Encrypt([]byte(path), key)
		dataInitLen := make([]byte, 2)
		binary.BigEndian.PutUint16(dataInitLen, uint16(len(dataInit)))
		d2r = append(dataInitLen, dataInit...)
	}
	handleRemote(client, shost, sport, rhost, rport, d2c, d2r, key)
}

func getRemoteInfo(shost string) (string, string, []byte) {
	rhost := ""
	rport := ""
	key := []byte("")
	if serverInList(shost) {
		rhost = gconfig.Host
		rport = gconfig.Port
		key = KEY
	} else {
		rhost = gconfig.DirectHost
		rport = gconfig.DirectPort
		key = DIRECT_KEY
	}
	return rhost, rport, key
}

func handleRemote(client net.Conn, shost, sport, rhost, rport string, d2c, d2r, key []byte) {
	remote, err := net.Dial("tcp", rhost+":"+rport)
	if err != nil {
		info("cannot connect to remote: %s:%s", rhost, rport)
		return
	}
	keyServer := fmt.Sprintf("%s:%s", shost, sport)
	initServers(keyServer, 0)
	defer func() {
		remote.Close()
		deleteServers(fmt.Sprintf("%s:%s", shost, sport))
		debug("closed remote for %s:%s", shost, sport)
	}()
	debug("connected to remote: %s", remote.RemoteAddr())

	bytesCheck := make([]byte, 8)
	copy(bytesCheck, key[8:16])
	bytesCheck = encrypt.Encrypt(bytesCheck, key)
	remote.Write([]byte{byte(len(bytesCheck))})
	remote.Write(bytesCheck)

	bytesHost := []byte(shost)
	bytesHost = encrypt.Encrypt(bytesHost, key)
	remote.Write([]byte{byte(len(bytesHost))})
	remote.Write(bytesHost)

	b := make([]byte, 2)
	nportServer, _ := strconv.Atoi(sport)
	binary.BigEndian.PutUint16(b, uint16(nportServer))
	remote.Write(b)

	ch_client := make(chan DataInfo)
	ch_remote := make(chan []byte)

	if d2c != nil {
		client.Write(d2c)
	}
	if d2r != nil {
		remote.Write(d2r)
	}

	go readDataFromClient(ch_client, ch_remote, client)
	go readDataFromRemote(ch_remote, remote, shost, sport, key)

	shouldStop := false
	for {
		if shouldStop {
			break
		}

		select {
		case data := <-ch_remote:
			if data == nil {
				shouldStop = true
				break
			}
			client.Write(data)
		case di := <-ch_client:
			if di.data == nil {
				shouldStop = true
				break
			}
			buffer := encrypt.Encrypt(di.data[:di.size], key)
			b := make([]byte, 2)
			binary.BigEndian.PutUint16(b, uint16(len(buffer)))
			remote.Write(b)
			remote.Write(buffer)
		case <-time.After(60 * time.Second):
			debug("timeout on %s:%s", shost, sport)
			return
		}
	}
}

func readDataFromClient(ch chan DataInfo, ch2 chan []byte, conn net.Conn) {
	for {
		data := make([]byte, 8192)
		n, err := conn.Read(data)
		if err != nil {
			ch <- DataInfo{nil, 0}
			ch2 <- nil
			return
		}
		debug("received %d bytes from client", n)
		verbose("client: %s", data[:n])
		ch <- DataInfo{data, n}
	}
}

func readDataFromRemote(ch chan []byte, conn net.Conn, shost, sport string, key []byte) {
	for {
		buffer := make([]byte, 2)
		_, err := io.ReadFull(conn, buffer)
		if err != nil {
			ch <- nil
			return
		}
		size := binary.BigEndian.Uint16(buffer)

		keyServer := fmt.Sprintf("%s:%s", shost, sport)
		incrServers(keyServer, int64(size))

		buffer = make([]byte, size)
		_, err = io.ReadFull(conn, buffer)
		if err != nil {
			ch <- nil
			return
		}
		data, err := encrypt.Decrypt(buffer, key)
		if err != nil {
			info("ERROR: cannot decrypt data from client")
			ch <- nil
			return
		}
		debug("[%s:%s] received %d bytes", shost, sport, len(data))
		verbose("remote: %s", data)
		ch <- data
	}
}

func loadDirects() []byte {
	usr, err := user.Current()
	if err != nil {
		fmt.Printf("user current: %v\n", err)
		os.Exit(2)
	}
	fileKey := path.Join(usr.HomeDir, ".lightsockskey")
	data, err := ioutil.ReadFile(fileKey)
	if err != nil {
		fmt.Printf("failed to load key file: %v\n", err)
		os.Exit(1)
	}
	s := strings.TrimSpace(string(data))
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func getRouterConfig() []byte {
	usr, err := user.Current()
	if err != nil {
		fmt.Printf("user current: %v\n", err)
		os.Exit(2)
	}
	fileKey := path.Join(usr.HomeDir, ".goixy/config.json")
	if _, err := os.Stat(fileKey); os.IsNotExist(err) {
		return nil
	}

	data, err := ioutil.ReadFile(fileKey)
	if err != nil {
		fmt.Printf("failed to load direct-servers file: %v\n", err)
		os.Exit(1)
	}
	return data
}

func info(format string, a ...interface{}) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	prefix := fmt.Sprintf("[%s][%d] ", ts, countConnected)
	fmt.Printf(prefix+format+"\n", a...)
}

func debug(format string, a ...interface{}) {
	if DEBUG || VERBOSE {
		info(format, a...)
	}
}

func verbose(format string, a ...interface{}) {
	if VERBOSE {
		info(format, a...)
	}
}

func byteInArray(b byte, A []byte) bool {
	for _, e := range A {
		if e == b {
			return true
		}
	}
	return false
}

func initServers(key string, bytes int64) {
	m := cmap.New()
	now := time.Now()
	m.Set("ts", now.Unix())
	m.Set("bytes", bytes)
	Servers.Set(key, m)
}

func incrServers(key string, n int64) {
	if m, ok := Servers.Get(key); ok {
		if tmp, ok := m.(cmap.ConcurrentMap).Get("bytes"); ok {
			m.(cmap.ConcurrentMap).Set("bytes", tmp.(int64)+n)
		}
	} else {
		initServers(key, n)
	}
}

func deleteServers(key string) {
	Servers.Remove(key)
}

func loadRouterConfig() {
	b := getRouterConfig()
	if b == nil {
		return
	}
	err := json.Unmarshal(b, &gconfig)
	if err != nil {
		fmt.Printf("Invalid Goixy Config: %v\n", err)
		os.Exit(2)
	}

	// init keys
	s := strings.TrimSpace(gconfig.Key)
	_tmp := sha256.Sum256([]byte(s))
	KEY = _tmp[:]
	if gconfig.DirectKey != "" {
		s = strings.TrimSpace(gconfig.DirectKey)
		_tmp = sha256.Sum256([]byte(s))
		DIRECT_KEY = _tmp[:]
	} else {
		DIRECT_KEY = KEY
	}
}

func serverInList(shost string) bool {
	for _, s := range gconfig.WhiteList {
		re := regexp.MustCompile(s)
		s := re.FindString(shost)
		if s != "" {
			return true
		}
	}
	return false
}

type DataInfo struct {
	data []byte
	size int
}

const ATYP_IPV4 = 1
const ATYP_DOMAIN = 3
const ATYP_IPV6 = 4
