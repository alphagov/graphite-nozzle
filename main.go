package main

// Inspired by the noaa firehose sample script
// https://github.com/cloudfoundry/noaa/blob/master/firehose_sample/main.go

import (
	"crypto/tls"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/cloudfoundry-community/go-cfclient"
	"github.com/cloudfoundry/noaa"
	"github.com/cloudfoundry/noaa/events"
	"github.com/pivotal-cf/graphite-nozzle/metrics"
	"github.com/pivotal-cf/graphite-nozzle/processors"
	"github.com/quipo/statsd"
	"gopkg.in/alecthomas/kingpin.v2"
)

var (
	dopplerEndpoint   = kingpin.Flag("doppler-endpoint", "Doppler endpoint").Default("wss://doppler.10.244.0.34.xip.io:443").OverrideDefaultFromEnvar("DOPPLER_ENDPOINT").String()
	apiEndpoint       = kingpin.Flag("api-endpoint", "API endpoint").Default("https://api.10.244.0.34.xip.io").OverrideDefaultFromEnvar("API_ENDPOINT").String()
	subscriptionId    = kingpin.Flag("subscription-id", "Id for the subscription.").Default("firehose").OverrideDefaultFromEnvar("SUBSCRIPTION_ID").String()
	statsdEndpoint    = kingpin.Flag("statsd-endpoint", "Statsd endpoint").Default("10.244.11.2:8125").OverrideDefaultFromEnvar("STATSD_ENDPOINT").String()
	statsdPrefix      = kingpin.Flag("statsd-prefix", "Statsd prefix").Default("mycf.").OverrideDefaultFromEnvar("STATSD_PREFIX").String()
	prefixJob         = kingpin.Flag("prefix-job", "Prefix metric names with job.index").Default("false").OverrideDefaultFromEnvar("PREFIX_JOB").Bool()
	username          = kingpin.Flag("username", "UAA username.").Default("").OverrideDefaultFromEnvar("USERNAME").String()
	password          = kingpin.Flag("password", "UAA password.").Default("").OverrideDefaultFromEnvar("PASSWORD").String()
	clientID          = kingpin.Flag("client-id", "Client ID.").Default("").OverrideDefaultFromEnvar("CLIENT_ID").String()
	clientSecret      = kingpin.Flag("client-secret", "Client Secret.").Default("").OverrideDefaultFromEnvar("CLIENT_SECRET").String()
	skipSSLValidation = kingpin.Flag("skip-ssl-validation", "Please don't").Default("false").OverrideDefaultFromEnvar("SKIP_SSL_VALIDATION").Bool()
	debug             = kingpin.Flag("debug", "Enable debug mode. This disables forwarding to statsd and prints to stdout").Default("false").OverrideDefaultFromEnvar("DEBUG").Bool()
	metricTemplate    = kingpin.Flag("metric-template", "The template that will form a new metric namespace.").Default("").OverrideDefaultFromEnvar("APP_GUID").String()

	authToken   string
	consumer    *noaa.Consumer
	runningApps []string
)

func authenticate(client *cfclient.Client) (authToken string, consumer *noaa.Consumer, err error) {
	// FIXME This works fine, however we need to make sure to refresh the token
	// manually as we're currently setting it once and expect to work all the
	// time.
	authToken, err = client.GetToken()
	if err != nil {
		return authToken, consumer, err
	}

	consumer = noaa.NewConsumer(*dopplerEndpoint, &tls.Config{InsecureSkipVerify: *skipSSLValidation}, nil)

	return authToken, consumer, nil
}

func main() {
	kingpin.Parse()

	// FIXME We should ignore the firehose for the time being, making Client ID
	// and Secret redundant.
	c := &cfclient.Config{
		ApiAddress:        *apiEndpoint,
		SkipSslValidation: *skipSSLValidation,
		Username:          *username,
		Password:          *password,
		ClientID:          *clientID,
		ClientSecret:      *clientSecret,
	}

	client, err := cfclient.NewClient(c)
	if err != nil {
		fmt.Println(err)
		os.Exit(-1)
	}

	httpStartStopProcessor := processors.NewHttpStartStopProcessor()
	valueMetricProcessor := processors.NewValueMetricProcessor()
	containerMetricProcessor := processors.NewContainerMetricProcessor()
	heartbeatProcessor := processors.NewHeartbeatProcessor()
	counterProcessor := processors.NewCounterProcessor()

	sender := statsd.NewStatsdClient(*statsdEndpoint, *statsdPrefix)
	sender.CreateSocket()

	var processedMetrics []metrics.Metric
	var proc_err error

	msgChan := make(chan *events.Envelope)

	go watchApps(client, msgChan)

	for msg := range msgChan {
		eventType := msg.GetEventType()

		// graphite-nozzle can handle CounterEvent, ContainerMetric, Heartbeat,
		// HttpStartStop and ValueMetric events
		switch eventType {
		case events.Envelope_ContainerMetric:
			processedMetrics, proc_err = containerMetricProcessor.Process(msg)
		case events.Envelope_CounterEvent:
			processedMetrics, proc_err = counterProcessor.Process(msg)
		case events.Envelope_Heartbeat:
			processedMetrics, proc_err = heartbeatProcessor.Process(msg)
		case events.Envelope_HttpStartStop:
			processedMetrics, proc_err = httpStartStopProcessor.Process(msg)
		case events.Envelope_ValueMetric:
			processedMetrics, proc_err = valueMetricProcessor.Process(msg)
		default:
			// do nothing
		}

		if proc_err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", proc_err.Error())
		} else {
			// TODO We'd like to implement the metric template somewhere about here.
			if !*debug {
				if len(processedMetrics) > 0 {
					for _, metric := range processedMetrics {
						var prefix string
						if *prefixJob {
							prefix = msg.GetJob() + "." + msg.GetIndex()
						}
						metric.Send(sender, prefix)
					}
				}
			} else {
				for _, msg := range processedMetrics {
					fmt.Println(msg)
				}
			}
		}
		processedMetrics = nil
	}
}

// AppMutex should consit of a lock and the map of applications.
type AppMutex struct {
	watch map[string]chan struct{}
	mutex *sync.Mutex
}

// TODO Implement an application watcher that will kill or start new goroutines
// if the need arises.
func watchApps(client *cfclient.Client, msgChan chan *events.Envelope) {
	// defer close(msgChan)

	applications := AppMutex{}
	applications.watch = make(map[string]chan struct{})
	applications.mutex = &sync.Mutex{}

	for {
		updateApps(client, applications, msgChan)
		time.Sleep(300)
	}
}

func updateApps(client *cfclient.Client, applications AppMutex, msgChan chan *events.Envelope) {
	applications.mutex.Lock()
	defer applications.mutex.Unlock()

	authToken, err := client.GetToken()
	if err != nil {
		fmt.Println(err)
		os.Exit(-1)
	}

	apps, err := client.ListApps()
	if err != nil {
		fmt.Println(err)
		os.Exit(-1)
	}

	for _, app := range apps {
		keyName := app.SpaceData.Entity.Name + "." + app.Name
		errorChan := make(chan error)

		if _, ok := applications.watch[keyName]; !ok {
			runningApps = append(runningApps, keyName)
			applications.watch[keyName] = make(chan struct{})
			fmt.Printf("\n%#v\n%#v\n%#v\n%#v\n%#v\n", app.Guid, authToken, msgChan, errorChan, applications.watch[keyName])
			go consumer.Stream(app.Guid, authToken, msgChan, errorChan, applications.watch[keyName])
		}

		for err := range errorChan {
			fmt.Fprintf(os.Stderr, "%v\n", err.Error())
		}
	}

	// This loop is just "converting" a slice of map keys.
	watchers := []string{}
	for k := range applications.watch {
		watchers = append(watchers, k)
	}

	for _, app := range difference(runningApps, watchers) {
		applications.watch[app] <- struct{}{}
		delete(applications.watch, app)
	}
}
