package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"math/rand"

	"github.com/nats-io/go-nats"
)

//
// TODO - this is way to big, seperate out into client, tls, clientmanager
// source files
//

//
// Global Constants and Variables
//
const (
	DefaultConnectWait    = 4 * time.Minute
	DefaultConfigFileName = "config.json"
	TestNameTag           = "[TESTNAME]"
	InstanceTag           = "[INSTANCE]"
	ClientNameTag         = "[CLIENTNAME]"
	HostnameTag           = "[HOSTNAME]"
)

var (
	trace    bool
	verbose  bool
	testDone int32
	endTimer *time.Timer
	hostname string
)

//
// Utility Functions
//
func verbosef(format string, v ...interface{}) {
	if verbose {
		out := fmt.Sprintf(format, v...)
		log.Printf("%v: %s", time.Now().Format("2016-04-08 15:04:05.00"), out)
	}
}

func printf(format string, v ...interface{}) {
	out := fmt.Sprintf(format, v...)
	log.Printf("%v: %s", time.Now().Format("2016-04-08 15:04:05.00"), out)
}

func isTestDone() bool {
	return atomic.LoadInt32(&testDone) != 0
}

const fsecs = float64(time.Second)

func rps(count int64, elapsed time.Duration) int {
	if count <= 0 {
		return 0
	}
	return int(float64(count) / (float64(elapsed) / fsecs))
}

//
// Configuration
//

// TODO:  Maybe move TLS into it's own struct?
// Config is the general test configuration.
type Config struct {
	Name                  string         `json:"name"`
	ServerURLs            string         `json:"url"`
	TestDur               string         `json:"duration"`
	ConnectTimeout        string         `json:"connect_timeout"`
	IntialConnectAttempts int            `json:"initial_connect_attempts"`
	OutputFile            string         `json:"output_file"`
	PrettyPrint           bool           `json:"prettyprint,omitempty"`
	MaxStartDelay         string         `json:"client_start_delay_max"`
	TLSClientCA           string         `json:"tlsca"`
	TLSClientCert         string         `json:"tlscert"`
	TLSClientKey          string         `json:"tlskey"`
	TLSCipherSuites       []string       `json:"tlsciphers"`
	UseTLS                bool           `json:"usetls"`
	Clients               []ClientConfig `json:"clients"`
}

// ClientConfig represents a client
type ClientConfig struct {
	Name           string            `json:"name"`
	Instances      int               `json:"instances"`
	UserName       string            `json:"username"`
	Password       string            `json:"password"`
	PubMsgSize     int               `json:"pub_msgsize"`
	PubRate        int               `json:"pub_msgs_sec"`
	PubMsgCount    int               `json:"pub_msgcount,omitempty"`
	PublishSubject string            `json:"pub_subject"`
	Subscriptions  []ClientSubConfig `json:"subscriptions"`
}

// ClientSubConfig represents subscriptions for a client
type ClientSubConfig struct {
	Count   int    `json:"count,omitempty"`
	Subject string `json:"subject"`
}

// GenerateDefaultConfigFile generates a default config file with
// one publisher and one subscriber
func GenerateDefaultConfigFile() ([]byte, error) {
	cfg := Config{}
	cfg.Name = "single_pub_sub"
	cfg.MaxStartDelay = "250ms"
	cfg.ServerURLs = "nats://localhost:4222"
	cfg.UseTLS = false
	cfg.TestDur = "10s"
	cfg.OutputFile = "results.json"
	cfg.PrettyPrint = true
	cfg.IntialConnectAttempts = 10
	cfg.TLSCipherSuites = append(cfg.TLSCipherSuites, "TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305")

	cfg.Clients = make([]ClientConfig, 2)

	subject := HostnameTag + "." + TestNameTag + ".foo." + InstanceTag

	cfg.Clients[0].Instances = 1
	cfg.Clients[0].Name = "publisher"
	cfg.Clients[0].PubMsgSize = 128
	cfg.Clients[0].PubRate = 1000
	cfg.Clients[0].PublishSubject = subject

	cfg.Clients[1].Instances = 1
	cfg.Clients[1].Name = "subscriber"
	cfg.Clients[1].Subscriptions = make([]ClientSubConfig, 1)
	cfg.Clients[1].Subscriptions[0].Subject = subject

	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("could not marshal json: %v", err)
	}

	err = ioutil.WriteFile(DefaultConfigFileName, raw, 0644)
	if err != nil {
		return nil, fmt.Errorf("could not write default config file: %v", err)
	}

	printf("Generated default configuration file %s.\n", DefaultConfigFileName)
	return raw, nil
}

// LoadConfiguration loads a server configuration.
func LoadConfiguration(filename string) (*Config, error) {
	raw, err := ioutil.ReadFile(filename)
	if err != nil {
		if filename == DefaultConfigFileName {
			raw, err = GenerateDefaultConfigFile()
		}
		if err != nil {
			return nil, err
		}
	}

	serverConfigs, err := getConfig(string(raw))
	if err != nil {
		log.Println(err.Error())
		os.Exit(1)
	}

	return serverConfigs, err
}

//
// Client
//

// Client represents a NATS streaming client
type Client struct {
	cm             *ClientManager
	config         *ClientConfig
	testconfig     *Config
	clientID       string
	instance       int
	nc             *nats.Conn
	payload        []byte
	publishCount   int64
	publishSubject string
	publishRate    int
	pubdelay       time.Duration
	pubdone        bool
	pubStartTime   time.Time
	pubStopTime    time.Time
	subCh          chan (bool)
	subs           []*ClientSub
	closed         int32 // atomic to avoid locking
	asCount        int32 // async error count
	dcCount        int32 // disconnect count
	rcCount        int32 // reconnect count
	errCount       int32 // other error count (publish/flush)
	connAttempts   int   // # of initial connection attempts
	connSuccess    bool
	connCreateDur  time.Duration // amount of time it takes to connect
}

// ClientSub is a client subscription
type ClientSub struct {
	subject   string
	sub       *nats.Subscription
	ch        chan (bool)
	received  int64
	max       int64
	isDone    bool
	startTime time.Time
	stopTime  time.Time
}

// GetReceivedCount returns the count of received messages
func (cs *ClientSub) GetReceivedCount() int64 {
	return atomic.LoadInt64(&cs.received)
}

// GetSubActualMsgsPerSec gets the actual received message rate
func (cs *ClientSub) GetSubActualMsgsPerSec() int {
	count := atomic.LoadInt64(&cs.received)
	if cs.isDone == false {
		return rps(count, time.Now().Sub(cs.startTime))
	}
	return rps(count, cs.stopTime.Sub(cs.startTime))
}

func (c *Client) processSubject(subject string) string {

	// if no tags, just return the subject.
	if strings.Contains(subject, "]") == false {
		return subject
	}

	// go through our replacements
	s := strings.Replace(subject, InstanceTag, strconv.Itoa(c.instance), -1)
	s = strings.Replace(s, TestNameTag, c.cm.config.Name, -1)
	s = strings.Replace(s, ClientNameTag, c.config.Name, -1)
	s = strings.Replace(s, HostnameTag, hostname, -1)
	return s
}

// NewClient returns a new client.
func NewClient(config *ClientConfig, instance int, cm *ClientManager) *Client {
	c := &Client{}
	c.cm = cm
	c.config = config
	c.clientID = fmt.Sprintf("%s.%d", c.config.Name, instance)
	c.instance = instance
	c.publishRate = config.PubRate
	c.publishSubject = c.processSubject(config.PublishSubject)

	if c.isPublisher() {
		c.pubdelay = time.Second / time.Duration(c.publishRate)
	}
	return c
}

func (c *Client) disconnectedHandler(nc *nats.Conn) {
	if nc.LastError() != nil {
		verbosef("connection %q has been unexpectedly disconnected: %v\n",
			nc.Opts.Name, nc.LastError())
	}
	atomic.AddInt32(&c.dcCount, 1)
}

func (c *Client) reconnectedHandler(nc *nats.Conn) {
	verbosef("connection %q reconnected to NATS Server at %q\n",
		nc.Opts.Name, nc.ConnectedUrl())
	atomic.AddInt32(&c.rcCount, 1)
}

func (c *Client) closedHandler(nc *nats.Conn) {
	verbosef("connection %q has been closed\n", nc.Opts.Name)
}

func (c *Client) errorHandler(nc *nats.Conn, sub *nats.Subscription, err error) {
	log.Fatalf("asynchronous error on connection %s, subject %s: %s\n",
		nc.Opts.Name, sub.Subject, err)
	atomic.AddInt32(&c.asCount, 1)
}

// Where we maintain all of the available ciphers
var cipherMap = map[string]uint16{
	"TLS_RSA_WITH_RC4_128_SHA":                tls.TLS_RSA_WITH_RC4_128_SHA,
	"TLS_RSA_WITH_3DES_EDE_CBC_SHA":           tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
	"TLS_RSA_WITH_AES_128_CBC_SHA":            tls.TLS_RSA_WITH_AES_128_CBC_SHA,
	"TLS_RSA_WITH_AES_128_CBC_SHA256":         tls.TLS_RSA_WITH_AES_128_CBC_SHA256,
	"TLS_RSA_WITH_AES_256_CBC_SHA":            tls.TLS_RSA_WITH_AES_256_CBC_SHA,
	"TLS_RSA_WITH_AES_256_GCM_SHA384":         tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
	"TLS_ECDHE_ECDSA_WITH_RC4_128_SHA":        tls.TLS_ECDHE_ECDSA_WITH_RC4_128_SHA,
	"TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA":    tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
	"TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA":    tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
	"TLS_ECDHE_RSA_WITH_RC4_128_SHA":          tls.TLS_ECDHE_RSA_WITH_RC4_128_SHA,
	"TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA":     tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA,
	"TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA":      tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	"TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256":   tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,
	"TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256": tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256,
	"TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA":      tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
	"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256":   tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256": tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384":   tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384": tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305":    tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
	"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305":  tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
}

func setCiphers(opts *nats.Options, cipherNames []string) error {
	// ignore if unset
	if len(cipherNames) == 0 {
		return nil
	}

	if opts.TLSConfig == nil {
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	csuites := opts.TLSConfig.CipherSuites
	for _, name := range cipherNames {
		cipher, exists := cipherMap[name]
		if !exists {
			return fmt.Errorf("Unrecognized cipher %s", name)
		}
		csuites = append(csuites, cipher)
	}
	return nil
}

func (c *Client) generateOptions() (*nats.Options, error) {
	cmcfg := c.cm.config

	opts := nats.DefaultOptions
	opts.Servers = strings.Split(cmcfg.ServerURLs, ",")
	for i, s := range opts.Servers {
		opts.Servers[i] = strings.Trim(s, " ")
	}

	opts.AsyncErrorCB = c.errorHandler
	opts.DisconnectedCB = c.disconnectedHandler
	opts.ReconnectedCB = c.reconnectedHandler
	opts.ClosedCB = c.closedHandler
	opts.User = c.config.UserName
	opts.Password = c.config.Password
	opts.Name = c.clientID
	opts.SubChanLen = 1024 * 1024
	opts.Timeout = c.cm.connectTimeout

	// TLS options
	opts.Secure = cmcfg.UseTLS
	if cmcfg.TLSClientCA != "" {
		if err := nats.RootCAs(cmcfg.TLSClientCert)(&opts); err != nil {
			return nil, fmt.Errorf("client CA error: %v", err)
		}
	}
	if cmcfg.TLSClientCert != "" {
		if err := nats.ClientCert(cmcfg.TLSClientCert, cmcfg.TLSClientKey)(&opts); err != nil {
			return nil, fmt.Errorf("client cert error: %v", err)
		}
		// it's a test so allow self signed certs
		opts.TLSConfig.InsecureSkipVerify = true
	}
	if err := setCiphers(&opts, c.cm.config.TLSCipherSuites); err != nil {
		return nil, fmt.Errorf("error setting cipher: %v", err)
	}

	return &opts, nil
}

func (c *Client) connect() error {
	var err error

	cmcfg := c.cm.config

	attempts := cmcfg.IntialConnectAttempts
	if attempts <= 0 {
		attempts = 1
	}

	opts, err := c.generateOptions()
	if err != nil {
		log.Fatal(err)
	}

	for i := 0; i < attempts; i++ {
		c.connAttempts++
		start := time.Now()
		c.nc, err = opts.Connect()
		if err == nil {
			c.connCreateDur = time.Since(start)
			c.connSuccess = true
			break
		}
		verbosef("%s:  attempt %d to connect to %s failed: %v\n", c.clientID, c.connAttempts, opts.Servers, err)
		time.Sleep(time.Duration(rand.Intn(500)+100) * time.Millisecond)
	}

	if err != nil {
		printf("%s:  failed to connect: %v\n", c.clientID, err)
	}

	// connected or not,we're done waiting for this client.
	c.cm.connectedWg.Done()

	return err
}

func (c *Client) close() {
	c.closeSubscriptions()
	c.nc.Close()
	c.setClosed()
}

func (c *Client) completeSubscribers() {
	if len(c.subs) > 0 {
		stime := time.Now()
		for _, s := range c.subs {
			s.stopTime = stime
			s.ch <- true
		}
	}
}

func (c *Client) createClientSubscription(configSub *ClientSubConfig) {
	var err error

	csub := &ClientSub{}
	csub.ch = make(chan bool)
	csub.max = int64(configSub.Count)

	csub.subject = c.processSubject(configSub.Subject)
	if err != nil {
		log.Fatalf("unable to process subscribe subject %q: %v", configSub.Subject, err)
	}

	// unique callback per sub
	mh := func(msg *nats.Msg) {
		val := atomic.AddInt64(&csub.received, 1)
		if val == 1 {
			csub.startTime = time.Now()
		}
		if trace {
			printf("%s: Received message %d on %s.\n", c.clientID,
				val, msg.Subject)
		}
		if (csub.max > 0 && val == csub.max) || isTestDone() {
			verbosef("%s: Done receiving messages on subject %s.", c.clientID, msg.Subject)
			csub.stopTime = time.Now()
			csub.ch <- true
		}
	}

	natsSub, err := c.nc.Subscribe(csub.subject, mh)
	if err != nil {
		log.Fatalf("Error creating subscription for %s: %v", csub.subject, err)
	}
	c.nc.Flush()

	csub.sub = natsSub
	c.subs = append(c.subs, csub)

	verbosef("%s: Subscribed to %s.\n", c.clientID, csub.subject)
}

func (c *Client) waitForSubscriptions() {
	for _, sub := range c.subs {
		<-sub.ch
	}
	c.cm.subDoneWg.Done()
	verbosef("%s: All messages received.", c.clientID)
}

func (c *Client) isSubscriber() bool {
	return len(c.config.Subscriptions) > 0
}

func (c *Client) isPublisher() bool {
	return c.publishRate > 0
}

func (c *Client) createSubscriptions() {
	for _, s := range c.config.Subscriptions {
		c.createClientSubscription(&s)
	}
}

func (c *Client) closeSubscriptions() {
	for _, s := range c.subs {
		if err := s.sub.Unsubscribe(); err != nil {
			printf("error closing subscription: %v", err)
		}
	}
}

// Stolen from nats-io/latency-testing.
func (c *Client) adjustAndSleep() {
	r := rps(c.publishCount, time.Since(c.pubStartTime))
	adj := c.pubdelay / 20 // 5%
	if adj == 0 {
		adj = 1 // 1ns min
	}
	if r < c.publishRate {
		c.pubdelay -= adj
	} else if r > c.publishRate {
		c.pubdelay += adj
	}
	if c.pubdelay < 0 {
		c.pubdelay = 0
	}
	time.Sleep(c.pubdelay)
}

func (c *Client) publishMessage(subject string) {
	atomic.AddInt64(&c.publishCount, 1)
	c.adjustAndSleep()
	err := c.nc.Publish(subject, c.payload)
	if err != nil {
		verbosef("%s: Error publishing: %v.\n", c.clientID, err)
		atomic.AddInt32(&c.errCount, 1)
	}
	if trace {
		printf("%s: Success sending msg # %d to %s.\n", c.clientID,
			atomic.LoadInt64(&c.publishCount), subject)
	}
}

// PublishMessages publishes client messages
func (c *Client) PublishMessages() {
	verbosef("%s: Started publishing %d msgs on subject %s.\n",
		c.clientID, c.publishCount, c.publishSubject)

	c.payload = c.cm.payloadBuffer[:c.config.PubMsgSize]
	c.pubStartTime = time.Now()

	count := c.config.PubMsgCount
	subject := c.publishSubject

	for i := 0; i < count || count == 0; i++ {
		c.publishMessage(subject)
		if isTestDone() {
			break
		}
	}

	if err := c.nc.Flush(); err != nil {
		printf("%s: error flushing: %v", c.clientID, err)
	}
	c.pubStopTime = time.Now()
	c.pubdone = true

	verbosef("%s: Publishing complete.\n", c.clientID)

	c.cm.pubDoneWg.Done()
}

// GetPublishCount returns the current count of published messages
func (c *Client) GetPublishCount() int64 {
	return atomic.LoadInt64(&c.publishCount)
}

// GetPublishActualMsgsPerSec returns the actual (not configured) msgs/sec
func (c *Client) GetPublishActualMsgsPerSec() int {
	count := atomic.LoadInt64(&c.publishCount)
	if !c.isClosed() {
		return rps(count, time.Now().Sub(c.pubStartTime))
	}

	return rps(count, c.pubStopTime.Sub(c.pubStartTime))
}

func (c *Client) startDelay() {
	smd := c.cm.config.MaxStartDelay
	if smd != "" {
		delayMax, err := time.ParseDuration(smd)
		if err != nil {
			log.Fatalf("Invalid start delay: %v", err)
		}
		if delayMax > 0 {
			d := time.Duration(rand.Int63n(delayMax.Nanoseconds()))
			verbosef("%s:  Delaying connect by %v\n", c.clientID, d)
			time.Sleep(d)
		}
	}
}

func (c *Client) isClosed() bool {
	return atomic.LoadInt32(&c.closed) == 1
}

func (c *Client) setClosed() {
	atomic.StoreInt32(&c.closed, 1)
}

// Run connects a client to to the NATS server, starts the subscribers
// and publishes messages.
func (c *Client) Run() error {

	c.startDelay()

	if err := c.connect(); err != nil {
		c.errCount++

		// for automated tests, we want a summary of the failures, so remove
		// the client from action.
		if c.isSubscriber() {
			c.cm.subStartedWg.Done()
			c.cm.subDoneWg.Done()
		}
		if c.isPublisher() {
			c.cm.pubDoneWg.Done()
		}
		c.setClosed()
		verbosef("%s:  unable to connect: %v", c.clientID, err)
		return nil
	}

	verbosef("%s: Connected.", c.clientID)

	if c.isSubscriber() {
		c.createSubscriptions()
		c.cm.subStartedWg.Done()
	}

	if c.isPublisher() {
		// wait for all other subscribing clients to start
		c.cm.subStartedWg.Wait()
		c.PublishMessages()
	}

	if c.isSubscriber() {
		c.waitForSubscriptions()
	}
	c.close()

	return nil
}

func usage() {
	log.Fatal("Usage: nats-client-sim [-cfg <config file>] [-V] [-DV]")
}

func getConfig(jsonString string) (*Config, error) {
	var config = &Config{}

	err := json.Unmarshal([]byte(jsonString), config)
	if err != nil {
		return nil, fmt.Errorf("could not unmarshal json: %v", err)
	}

	return config, nil
}

//
// Client Management
//

// ClientManager tracks all clients
type ClientManager struct {
	sync.Mutex
	clients             []*Client
	config              *Config
	pubCount            int
	subCount            int
	connectedWg         sync.WaitGroup
	subStartedWg        sync.WaitGroup
	pubDoneWg           sync.WaitGroup
	subDoneWg           sync.WaitGroup
	payloadBuffer       []byte
	perfStartTime       time.Time
	perfEndTime         time.Time
	connectTimeout      time.Duration
	printDetailedReport bool
}

func printClient(c *Client) {
	verbosef("%s: Created.  pubsubj=%s,pubcount=%d,msgsize=%d,sub=%s,subcount=%d",
		c.clientID, c.publishSubject,
		c.config.PubMsgCount,
		c.config.PubMsgSize, "",
		len(c.config.Subscriptions))
}

func parseConnectTimeout(connTimeout string) time.Duration {
	if connTimeout == "" {
		return nats.DefaultTimeout
	}
	timeOut, err := time.ParseDuration(connTimeout)
	if err != nil {
		log.Fatalf("unable to parse connect_timeout: %v", err)
	}
	return timeOut
}

// NewClientManager creates a client manager
func NewClientManager(cfg *Config, detailedReport bool) *ClientManager {
	var maxMsgSize int

	// print out general info
	// TODO - move this out...
	if cfg.UseTLS {
		verbosef("Using TLS.\n")
	}
	if cfg.TLSClientCA != "" {
		verbosef("Using client CA %s\n", cfg.TLSClientCA)
	}
	if cfg.TLSClientCert != "" {
		verbosef("Using client cert: %s\n", cfg.TLSClientCert)
		verbosef("Using client key: %s\n", cfg.TLSClientKey)
	}

	cm := &ClientManager{
		config:              cfg,
		printDetailedReport: detailedReport,
		connectTimeout:      parseConnectTimeout(cfg.ConnectTimeout),
	}

	for i := 0; i < len(cfg.Clients); i++ {
		for j := 0; j < cfg.Clients[i].Instances; j++ {
			cli := NewClient(&cfg.Clients[i], j, cm)
			if cli.isPublisher() {
				cm.pubCount++
				if cfg.Clients[i].PubMsgSize > maxMsgSize {
					maxMsgSize = cfg.Clients[i].PubMsgSize
				}
			}

			if cli.isSubscriber() {
				cm.subCount++
			}

			cm.clients = append(cm.clients, cli)
			printClient(cli)
		}
	}

	cm.payloadBuffer = make([]byte, maxMsgSize)

	printf("Creating %d simulated clients:  %d publishing / %d subscribing.\n",
		len(cm.clients), cm.pubCount, cm.subCount)

	return cm
}

// RunClients runs all the configured clients
func (cm *ClientManager) RunClients() {
	cm.connectedWg.Add(len(cm.clients))
	cm.subStartedWg.Add(cm.subCount)
	cm.pubDoneWg.Add(cm.pubCount)
	cm.subDoneWg.Add(cm.subCount)

	for _, c := range cm.clients {
		go c.Run()
	}
	printf("Clients connecting.\n")
}

// WaitForCompletion waits until all clients have been completed.
func (cm *ClientManager) WaitForCompletion() {
	cm.connectedWg.Wait()
	cm.subStartedWg.Wait()

	printf("Publishers starting.")

	if cm.config.TestDur != "" {
		dur, err := time.ParseDuration(cm.config.TestDur)
		if err != nil {
			log.Fatalf("Unable to parse duration: %s", dur)
		}
		endTimer = time.NewTimer(dur)
		go func() {
			<-endTimer.C
			atomic.AddInt32(&testDone, 1)

			// stop the subscriptions
			for _, c := range cm.clients {
				c.completeSubscribers()
			}
		}()
	}

	// subscribers are ready and publishing will commence,
	// so start measuring throughput
	cm.perfStartTime = time.Now()
	cm.pubDoneWg.Wait()
	cm.subDoneWg.Wait()
	cm.perfEndTime = time.Now()

	printf("All clients finished.")
}

func (cm *ClientManager) printAggregateMsgRate(msgsSent, msgsRecv int64) {
	d := time.Now().Sub(cm.perfStartTime)
	msRate := rps(msgsSent, d)
	mrRate := rps(msgsRecv, d)

	printf("Sent aggregate %d msgs at %d msgs/sec.\n", msgsSent, int(msRate))
	printf("Received aggregate %d msgs at %d msgs/sec.\n", msgsRecv, int(mrRate))
}

func (cm *ClientManager) displayClientsAndRates() {
	var line string
	var count int

	cm.Lock()
	defer cm.Unlock()

	for _, c := range cm.clients {
		line = fmt.Sprintf("%v: Client %s,", time.Now().Format("2016-04-08 15:04:05"), c.clientID)
		if c.isPublisher() {
			line += fmt.Sprintf(" pub: %s=(%d/%d),(%d msgs/sec)", c.config.PublishSubject,
				c.GetPublishCount(), c.config.PubMsgCount, c.GetPublishActualMsgsPerSec())
		}

		if c.isSubscriber() {
			line += " subs:"
			for _, csub := range c.subs {
				line += fmt.Sprintf(" %s=(%d/%d),(%d msgs/sec)", csub.subject,
					csub.GetReceivedCount(), csub.max, csub.GetSubActualMsgsPerSec())
			}
		}

		log.Printf("%s\n", line)
		count++
	}
}

// displayClientsAndRates runs a report of current active clients
func (cm *ClientManager) displayRates() {
	var tsent int64
	var trecv int64

	cm.Lock()
	defer cm.Unlock()

	for _, c := range cm.clients {
		if c.isPublisher() {
			tsent += c.GetPublishCount()
		}

		if c.isSubscriber() {
			for _, csub := range c.subs {
				trecv += csub.GetReceivedCount()
			}
		}
	}

	cm.printAggregateMsgRate(tsent, trecv)
}

//
// Output File
//

// SummaryRecord provides a summary of the test.
type SummaryRecord struct {
	Type              string `json:"type"`
	TestName          string `json:"testname"`
	Hostname          string `json:"hostname"`
	CfgDuration       string `json:"duration"`
	ActDuration       string `json:"active_duration"`
	TLS               bool   `json:"tls"`
	NumClients        int    `json:"client_count"`
	NumPublishers     int    `json:"pub_count"`
	NumSubscribers    int    `json:"sub_count"`
	TotalErrors       int    `json:"error_count"`
	TotalDisconnects  int    `json:"disconnect_count"`
	TotalAsErrors     int    `json:"as_error_count"`
	TotalConnects     int    `json:"connect_count"`
	TotalReconnects   int    `json:"reconnect_count"`
	TotalMessagesSent int    `json:"msgs_sent"`
	TotalMessagesRecv int    `json:"msgs_recv"`
	AvgConnAttempts   int    `json:"avg_conn_attempts"`
	AvgConnConnectDur string `json:"avg_conn_connect_dur"`
}

func avgDurs(v *[]time.Duration) string {
	var ttl time.Duration
	count := len(*v)
	if count == 0 {
		return "0s"
	}
	for _, d := range *v {
		ttl += d
	}
	return (ttl / time.Duration(count)).String()
}

// NewSummaryRecord generates a new summary record for writing
func (cm *ClientManager) NewSummaryRecord() *SummaryRecord {
	var (
		asCount       int // async error count
		dcCount       int // disconnect count
		rcCount       int // reconnect count
		errCount      int // other error count (publish/flush)
		tConnAttempts int // connection attempts
		tConnections  int // connection successes
		numPubs       int // number of publishers
		numSubs       int // number of subscribers
		tSent         int // number of sent messages
		tRecv         int // number of recv messages
	)

	var durs []time.Duration
	for _, c := range cm.clients {
		asCount += int(c.asCount)
		dcCount += int(c.dcCount)
		rcCount += int(c.rcCount)
		errCount += int(c.errCount)
		tConnAttempts += c.connAttempts

		if c.connSuccess {
			tConnections++
			durs = append(durs, c.connCreateDur)
		}

		if c.isPublisher() {
			numPubs++
			tSent += int(c.GetPublishCount())
		}

		if c.isSubscriber() {
			numSubs += len(c.config.Subscriptions)
			for _, s := range c.subs {
				tRecv += int(s.GetReceivedCount())
			}
		}
	}

	sr := &SummaryRecord{
		Type:              "summary",
		TestName:          cm.config.Name,
		Hostname:          hostname,
		CfgDuration:       cm.config.TestDur,
		ActDuration:       cm.perfEndTime.Sub(cm.perfStartTime).String(),
		TLS:               cm.config.UseTLS,
		TotalConnects:     tConnections,
		TotalErrors:       errCount,
		TotalDisconnects:  dcCount,
		TotalReconnects:   rcCount,
		TotalAsErrors:     asCount,
		NumClients:        len(cm.clients),
		NumPublishers:     numPubs,
		NumSubscribers:    numSubs,
		TotalMessagesSent: tSent,
		TotalMessagesRecv: tRecv,
		AvgConnConnectDur: avgDurs(&durs),
		AvgConnAttempts:   tConnAttempts / len(cm.clients),
	}
	return sr
}

// SubRecord is a record for a subscription
type SubRecord struct {
	Subject   string `json:"name"`
	RecvCount int    `json:"msgs_recv"`
	Rate      int    `json:"msgs_per_sec"`
}

// ClientRecord is the record of a client
type ClientRecord struct {
	Type              string      `json:"type"`
	Name              string      `json:"name"`
	Instance          int         `json:"instance"`
	ConnectAttempts   int         `json:"conn_attempts"`
	ConnectTime       string      `json:"connect_time"`
	Errors            int         `json:"error_count"`
	Reconnects        int         `json:"reconnect_count"`
	Disconnects       int         `json:"disconnect_count"`
	AsyncErrors       int         `json:"async_error_count"`
	PublishRate       int         `json:"publish_msgs_per_sec"`
	PublishSubj       string      `json:"publish_subject"`
	MessagesSent      int         `json:"msgs_sent"`
	TotalMessagesRecv int         `json:"msgs_recv"`
	NumSubscribers    int         `json:"sub_count"`
	Subscribers       []SubRecord `json:"subs"`
}

// NewClientRecord generates a client record for writing to a file
func (cm *ClientManager) NewClientRecord(c *Client) *ClientRecord {
	var trecv int

	cr := &ClientRecord{
		Type:            "client",
		Name:            c.config.Name,
		Instance:        c.instance,
		ConnectAttempts: int(c.connAttempts),
		Errors:          int(c.errCount),
		Reconnects:      int(c.rcCount),
		AsyncErrors:     int(c.asCount),
		PublishRate:     c.GetPublishActualMsgsPerSec(),
		MessagesSent:    int(c.publishCount),
		PublishSubj:     c.publishSubject,
		NumSubscribers:  len(c.subs),
	}

	scount := len(c.subs)
	if scount > 0 {
		cr.Subscribers = make([]SubRecord, scount)
		for i := 0; i < scount; i++ {
			s := c.subs[i]
			trecv += int(s.received)
			cr.Subscribers[i].Subject = s.subject
			cr.Subscribers[i].Rate = rps(s.received, s.stopTime.Sub(s.startTime))
			cr.Subscribers[i].RecvCount = int(s.GetReceivedCount())
		}
	}

	return cr
}

func (cm *ClientManager) marshalObj(v interface{}) ([]byte, error) {
	var (
		raw []byte
		err error
	)

	if cm.config.PrettyPrint {
		raw, err = json.MarshalIndent(v, "", "    ")
	} else {
		if raw, err = json.Marshal(v); err != nil {
			return nil, err
		}
		raw = append(raw, '\n')
	}
	return raw, err
}

// JSONOutput is the pretty generated JSON output
type JSONOutput struct {
	Summary *SummaryRecord  `json:"summary"`
	Clients []*ClientRecord `json:"clients"`
}

func (cm *ClientManager) writePretty(f *os.File) {
	count := len(cm.clients)

	jo := &JSONOutput{
		Summary: cm.NewSummaryRecord(),
		Clients: make([]*ClientRecord, count),
	}
	for i := 0; i < count; i++ {
		jo.Clients[i] = cm.NewClientRecord(cm.clients[i])
	}
	raw, err := cm.marshalObj(jo)
	if err != nil {
		log.Fatalf("Couldn't marshal output: %v", err)
	}
	f.Write(raw)
}

func (cm *ClientManager) writeDevops(f *os.File) {
	sr := cm.NewSummaryRecord()
	raw, err := cm.marshalObj(sr)
	if err != nil {
		log.Fatalf("Couldn't marshal output: %v", err)
	}
	f.Write(raw)

	for _, c := range cm.clients {
		cr := cm.NewClientRecord(c)
		raw, err := cm.marshalObj(cr)
		if err != nil {
			log.Fatalf("Couldn't marshal output: %v", err)
		}
		f.Write(raw)
	}
}

func (cm *ClientManager) writeOutputFile() error {
	of := cm.config.OutputFile
	if of == "" {
		return nil
	}

	printf("Writing output file %q.\n", of)

	f, err := os.Create(of)
	if err != nil {
		return err
	}
	defer f.Close()

	if cm.config.PrettyPrint {
		cm.writePretty(f)
	} else {
		cm.writeDevops(f)
	}

	return nil
}

//
// Application flow
//

func (cm *ClientManager) printBanner() {
	cfg := cm.config

	of := cfg.OutputFile
	if of == "" {
		of = "(none)"
	}
	printf("Test Name:   %s\n", cfg.Name)
	printf("URLs:        %s\n", cfg.ServerURLs)
	printf("Output File: %s\n", of)
	printf("# Clients:   %d\n", len(cm.clients))
	printf("Duration:    %s\n", cfg.TestDur)
	printf("===================================")
}

// run runs the application
func Run(configFile string, isVerbose, isTraceVerbose, longReport bool) {
	var err error

	// for testing
	atomic.StoreInt32(&testDone, 0)

	verbose = isVerbose
	if isTraceVerbose {
		verbose = true
		trace = true
	}

	if hostname, err = os.Hostname(); err != nil {
		log.Fatalf("error getting hostname:  %v\n", err)
	}

	cfg, err := LoadConfiguration(configFile)
	if err != nil {
		log.Fatalf("error loading configuration file:  %v\n", err)
	}

	cman := NewClientManager(cfg, longReport)
	cman.printBanner()
	cman.RunClients()
	cman.WaitForCompletion()
	endTimer.Stop()

	if longReport {
		cman.displayClientsAndRates()
	}
	cman.displayRates()

	printf("Test complete.")
	if err := cman.writeOutputFile(); err != nil {
		log.Fatalf("couldn't write output file: %v", err)
	}
}

func main() {
	var configFile = flag.String("config", DefaultConfigFileName, "configuration file to use.  Default is generated.")
	var vb = flag.Bool("V", false, "Verbose")
	var tb = flag.Bool("DV", false, "Verbose/Trace")
	var pr = flag.Bool("report", false, "Print a long form of report data.")

	log.SetFlags(0)
	flag.Parse()

	Run(*configFile, *vb, *tb, *pr)
}
