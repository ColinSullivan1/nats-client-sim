# The NATS Client Simulator

The NATS Client Simulator is a configuration file driven test application that simulates a NATS client ecosystem.  In a single configuration, mulitple "clients" can be configured to publish, subscribe, or both.  A client is simply a goroutine that can publish at a specified rate with a specified message size.  and the simluator runs for a configured duration.  Multiple instances of clients can be launched to easily test large number clients at scale.

This was designed as a tool to help identify the best machine image / resource choice for sizing NATS cloud deployments.

## Installation

```bash
> go get github.com/ColinSullivan1/nats-client-sim
```

## Usage

The NATS client simulator is configuration file driven allowing you to save various configurations to test and compare against different machine images, network settings, cluster sizes, etc.

```text
Usage of ./nats-client-sim:
  -V              Print verbose output.  This will affect performance of the test.
  -config <file>  Configuration file to use.  Default is generated. (default "config.json")
  -report         Print a long form of report data.
```

If no configuration file is specfied, a local configuration file, `config.json` is used.  If `config.json` is not present, a configuration is generated that simulates a publisher app and a subscriber app producing and consuming on a unique stream.  This is a convenient way to start your configuration.  After a test has been run, a `results.json` file is generated containing a summary and results of all simulated clients.

### Configuration Files

Let's dissect the default configuration file:

```text
{
  "name": "single_pub_sub",          ## Name of the test, saved in results.json 
  "url": "nats://localhost:4222",    ## The urls the clients will use
  "duration": "10s",                 ## Duration of the test (once all clients have connected)
  "connect_timeout": "",             ## NATS Timeout - a connect timeout.
  "initial_connect_attempts": 10,    ## Number of connect attepts before giving up.
  "output_file": "results.json",     ## Name of the output file
  "prettyprint": true,               ## Print in JSON (vs a "devops" JSON format).
  "client_start_delay_max": "250ms", ## Random start delay for each client.
  "tlsca": "",                       ## TLS certificate authority
  "tlscert": "",                     ## TLS certificate
  "tlskey": "",                      ## TLS private key (for all clients)
  "tlsciphers": [                    ## Client cipher suite to use.  See `gnatsd --help_tls`.
    "TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305"
  ],
  "usetls": false,                   ## Do not use client side certificates.
  "clients": [                       ## Array of clients
    {
      "name": "publisher",           ## Client name
      "instances": 1,                ## Number of instances of this client
      "username": "",                ## NATS connection user name
      "password": "",                ## NATS connection password
      "pub_msgsize": 128,            ## Publishing message size
      "pub_msgs_sec": 1000,          ## Publish rate (msgs per sec).  IF ZERO, do not publish.
      "pub_subject": "[HOSTNAME].[TESTNAME].foo.[INSTANCE]", ## Subject
      "subscriptions": null          ## NOTE:  This client will not receive any messages.
    },
    {
      "name": "subscriber",
      "instances": 1,
      "username": "",
      "password": "",
      "pub_msgsize": 0,
      "pub_msgs_sec": 0,             ## NOTE:  This client does not publish
      "pub_subject": "",
      "subscriptions": [             ## An array of subscriptions.
        {
          "subject": "[HOSTNAME].[TESTNAME].foo.[INSTANCE]"
          "count" : 1                ## Number of subscriptions to create
        }
      ]
    }
  ]
}
```

This application creates one publisher, and one subscriber that will create a stream of 1000, 128b messages per second.  Payload data is randomized.

If one wanted to scale out, one could create multiple instances of subscribers to simulate fanout.  Subjects can be any supported NATS subjects.

### Tags

To faciliate generating unique streams across multiple boxes, tags can be used which expand into other values.  These are useful to isolate data streamsfor simulating low volume high fanout alongside a highthoughput stream.

* `[TESTNAME]` - name of the test as described by the toplevel `"name"` json field
* `[INSTANCE]` - instance # of the client
* `[CLIENTNAME]` - the name of the client
* `[HOSTNAME]` - the hostname of the node the test is running on.

So for example, if I wanted to create a stream with a client on the same host, I could publsh to `foo.[HOSTNAME]`.  This might be translated into `foo.serverb`, and only clients on serverb would receive the messages.  Using a combination of testname, instance, clientname, and hostname will guarantee a unique stream per client if you wanted to scale instances of nats-client-sim out across mulitple machine instances.

## Examples

* [4 Unique Streams](configs/4streams.json) - Four publishers paired with four subscribers utilizing tags to create 8 simulated clients using four unique streams, each at 1000msgs/sec for three seconds.
* [High Fanout](configs/fanout_1to100.json) - 1 publisher publishing 100 subscribers on a simple subject, `foo`.
* [Simple](configs/simple.json) - A single application that publishes one stream of data to itself (a local subscriber).
* [TLS with cipher](configs/tls_with_cipher.json) - TLS enabled with clients selecting a specific cipher to use.
* [TLS](configs/tls.json) - A simple TLS setup


These example configurations are used in the tests.

## Application flow

The test application behaves as follows:

1. All clients connect (some may fail)
2. All subscribing clients create their subscriptions
3. All publishing clients start publishing.  The test duration timer starts.
4. Messages flow!
5. The test duration timer expires and a results file is generated.  If specfied, a report is printed to stdout with a summary of the test and each client.

## Notable configuration parameters

In testing at scale, these configuration parameters are ones you'll want to look into.

`"client_start_delay_max"` - When configured with many instances of applications (e.g. 10000), you may want to simulate a soft startup to avoid TLS connection errors related to CPU spikes in the server.  What this parameter does is delay the start of each client by a duration between zero and this value.  This randomizes connection attempts over a period of time.  If attempting thousands of TLS connections simultaneously on a server with only a few cores, a value of "60s" isn't unreasonable to allow the test to succeed.

`"connect_timeout"` - This is the connection timeout of each NATS client.  If a server is stressed at connection time, the client may timeout waiting for a NATS connection to be established.  This value can be increased, for example `"10s"` to test what you'll need in production.

`"connect_attempts` - During the connection phase of the test, clients will keep connecting until successful or until they've exceeding this value.  Note that with a high value here and a high value for connect timeout may mean a test will run much longer than specified by the duration.

`"test_duration`" -  How long the test will run, e.g. `1s`, `5m`, `12h` etc.

## Output

```text
10106-49-08 15:49:27.03: Creating 2 simulated clients:  1 publishing / 1 subscribing.
10106-49-08 15:49:27.03: Test Name:   single_pub_sub
10106-49-08 15:49:27.03: URLs:        nats://localhost:4222
10106-49-08 15:49:27.03: Output File: results.json
10106-49-08 15:49:27.03: # Clients:   2
10106-49-08 15:49:27.03: Duration:    10s
10106-49-08 15:49:27.03: ===================================
10106-49-08 15:49:27.03: Clients connecting.
10106-49-08 15:49:27.22: Publishers starting.
10106-49-08 15:49:37.22: All clients finished.
10106-49-08 15:49:37.22: Sent aggregate 10103 msgs at 1010 msgs/sec.
10106-49-08 15:49:37.22: Received aggregate 10102 msgs at 1010 msgs/sec.
10106-49-08 15:49:37.22: Test complete.
10106-49-08 15:49:37.22: Writing output file "results.json".
```

## Results

If specified, a results file is generated.  We'll dissect the output...

```text
{
    "summary": {
        "type": "summary",                  ## Type of JSON record
        "testname": "single_pub_sub",       ## Name of the test
        "timestamp": "2018-10-11 10:50...", ## Timestamp of the test
        "hostname": "MacBook-Pro.local",    ## Hostname this app ran on
        "duration": "10s",                  ## Simulation duration target
        "active_duration": "10.001797142s", ## Actual simulation duration
        "tls": false,                       ## TLS was used
        "client_count": 2,                  ## Number of clients
        "pub_count": 1,                     ## Number of publishers
        "sub_count": 1,                     ## Number of Subscribers
        "error_count": 0,                   ## Number of general errors
        "disconnect_count": 2,              ## Disconnect count (usually 1 per client)
        "as_error_count": 0,                ## Async error count
        "connect_count": 2,                 ## Number of clients that connected
        "reconnect_count": 0,               ## Number of clients that reconnected
        "msgs_sent": 10103,                 ## Total number of messages sent
        "msgs_recv": 10102,                 ## Total number of messages received
        "avg_conn_attempts": 1,             ## Average # of connect attempts per client
        "avg_conn_connect_dur": "8.092154ms"  ## Average conect time (duration to connect)
    },
    "clients": [
        {
            "type": "client",               ## This is a client record
            "name": "publisher",            ## Client Name
            "instance": 0,                  ## Instance # of the client
            "conn_attempts": 1,             ## Connect attepts for this client
            "connect_time": "",             ## Client connect time.
            "error_count": 0,               ## Number of general errors
            "reconnect_count": 0,           ## Number of times this client reconnected
            "disconnect_count": 1,          ## Number of times this client disconnected
            "async_error_count": 0,         ## Asyncronous errors (slow consumers, auth)
            "publish_msgs_per_sec": 1010,   ## Actual published messages per second
            "publish_subject": "MacBook-Pro.local.single_pub_sub.foo.0", ## subject
            "msgs_sent": 10103,             ## Actual number of messages sent
            "msgs_recv": 0,                 ## Total number of messages received
            "sub_count": 0,                 ## Total number of subscribers
            "subs": null
        },
        {
            "type": "client",
            "name": "subscriber",
            "instance": 0,
            "conn_attempts": 1,
            "connect_time": "",
            "error_count": 0,
            "reconnect_count": 0,
            "disconnect_count": 1,
            "async_error_count": 0,
            "publish_msgs_per_sec": 0,
            "publish_subject": "",
            "msgs_sent": 0,
            "msgs_recv": 0,
            "sub_count": 1,
            "subs": [  ## array of subscribers
                {
                    "name": "MacBook-Pro.local.single_pub_sub.foo.0",  ## Subscriber name/subject
                    "msgs_recv": 10102,   ## Number of messages received
                    "msgs_per_sec": 1010  ## Receive rate
                }
            ]
        }
    ]
```

From here, we can see there were no asychronous errors (usually slow consumers), and we see that the subscriber was keeping up with the publisher, so this is a sustainable system.

## TODO

[ ] Break into muliple source files (client, client manager, main, etc.)
[ ] Better test timing - create a connect timer to give up connecting and continue after a period of time.