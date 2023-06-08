package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/Shopify/sarama"
)

func init() {
	sarama.Logger = log.New(os.Stdout, "[Sarama] ", log.LstdFlags)
}

var (
	brokers       = flag.String("brokers", os.Getenv("KAFKA_PEERS"), "The Kafka brokers to connect to, as a comma separated list")
	userName      = flag.String("username", "", "The SASL username")
	passwd        = flag.String("passwd", "", "The SASL password")
	algorithm     = flag.String("algorithm", "", "The SASL SCRAM SHA algorithm sha256 or sha512 as mechanism")
	topic         = flag.String("topic", "default_topic", "The Kafka topic to use")
	certFile      = flag.String("certificate", "", "The optional certificate file for client authentication")
	keyFile       = flag.String("key", "", "The optional key file for client authentication")
	caFile        = flag.String("ca", "", "The optional certificate authority file for TLS client authentication")
	tlsSkipVerify = flag.Bool("tls-skip-verify", true, "Whether to skip TLS server cert verification")
	useTLS        = flag.Bool("tls", true, "Use TLS to communicate with the cluster")
	mode          = flag.String("mode", "consume", "Mode to run in: \"produce\" to produce, \"consume\" to consume")
	logMsg        = flag.Bool("logmsg", true, "True to log consumed messages to console")

	logger = log.New(os.Stdout, "[Producer] ", log.LstdFlags)
)

func createTLSConfiguration() (t *tls.Config) {
	t = &tls.Config{
		InsecureSkipVerify: *tlsSkipVerify,
	}
	if *certFile != "" && *keyFile != "" && *caFile != "" {
		cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			log.Fatal(err)
		}

		caCert, err := os.ReadFile(*caFile)
		if err != nil {
			log.Fatal(err)
		}

		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)

		t = &tls.Config{
			Certificates:       []tls.Certificate{cert},
			RootCAs:            caCertPool,
			InsecureSkipVerify: *tlsSkipVerify,
		}
	}
	return t
}

func main() {
	flag.Parse()

	if *brokers == "" {
		log.Fatalln("at least one broker is required")
	}
	splitBrokers := strings.Split(*brokers, ",")

	if *userName == "" {
		log.Fatalln("SASL username is required")
	}

	if *passwd == "" {
		log.Fatalln("SASL password is required")
	}

	conf := sarama.NewConfig()
	conf.Producer.Retry.Max = 1
	conf.Producer.RequiredAcks = sarama.WaitForAll
	conf.Producer.Return.Successes = true
	conf.Metadata.Full = true
	conf.Version = sarama.V0_10_2_0
	conf.ClientID = "sasl_scram_client"
	conf.Metadata.Full = true
	conf.Net.SASL.Enable = true
	conf.Net.SASL.User = *userName
	conf.Net.SASL.Password = *passwd
	conf.Net.SASL.Handshake = true
	if *algorithm == "sha512" {
		conf.Net.SASL.SCRAMClientGeneratorFunc = func() sarama.SCRAMClient { return &XDGSCRAMClient{HashGeneratorFcn: SHA512} }
		conf.Net.SASL.Mechanism = sarama.SASLTypeSCRAMSHA512
	} else if *algorithm == "sha256" {
		conf.Net.SASL.SCRAMClientGeneratorFunc = func() sarama.SCRAMClient { return &XDGSCRAMClient{HashGeneratorFcn: SHA256} }
		conf.Net.SASL.Mechanism = sarama.SASLTypeSCRAMSHA256

	} else {
		log.Fatalf("invalid SHA algorithm \"%s\": can be either \"sha256\" or \"sha512\"", *algorithm)
	}

	if *useTLS {
		conf.Net.TLS.Enable = true
		conf.Net.TLS.Config = createTLSConfiguration()
	}

	client, err := sarama.NewClient(splitBrokers, conf)
	if err != nil {
		log.Fatalf("unable to create kafka client: %q\n", err)
	}
	fmt.Print("client created", client)

	ctx, cancel := context.WithCancel(context.Background())
	consumerClient, err := sarama.NewClient(splitBrokers, conf)
	if err != nil {
		log.Panicf("Error creating consumer client: %v", err)
	}
	defer func(consumerClient sarama.Client) {
		err := consumerClient.Close()
		if err != nil {
			print(err)
		}
	}(consumerClient)

	consumerGroup, err := sarama.NewConsumerGroupFromClient("knative-consumer-group-name", consumerClient)
	if err != nil {
		log.Panicf("Error creating consumer group client: %v", err)
	}

	consumer := Consumer{
		ready: make(chan bool),
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			// `Consume` should be called inside an infinite loop, when a
			// server-side rebalance happens, the consumer session will need to be
			// recreated to get the new claims
			topics := strings.Split(*topic, ",")
			if err := consumerGroup.Consume(ctx, topics, &consumer); err != nil {
				log.Panicf("Error from consumer: %v", err)
			}
			// check if context was cancelled, signaling that the consumer should stop
			if ctx.Err() != nil {
				return
			}
			consumer.ready = make(chan bool)
		}
	}()

	<-consumer.ready // Await till the consumer has been set up
	log.Println("Sarama consumer up and running!...")

	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-ctx.Done():
		log.Println("terminating: context cancelled")
	case <-sigterm:
		log.Println("terminating: via signal")
	}
	cancel()
	wg.Wait()
	if err = client.Close(); err != nil {
		log.Panicf("Error closing client: %v", err)
	}

	logger.Println("Bye now!")
}

// Consumer represents a Sarama consumer group consumer
type Consumer struct {
	ready chan bool
}

// Setup is run at the beginning of a new session, before ConsumeClaim
func (consumer *Consumer) Setup(sarama.ConsumerGroupSession) error {
	// Mark the consumer as ready
	close(consumer.ready)
	return nil
}

// Cleanup is run at the end of a session, once all ConsumeClaim goroutines have exited
func (consumer *Consumer) Cleanup(sarama.ConsumerGroupSession) error {
	return nil
}

// ConsumeClaim must start a consumer loop of ConsumerGroupClaim's Messages().
func (consumer *Consumer) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	// NOTE:
	// Do not move the code below to a goroutine.
	// The `ConsumeClaim` itself is called within a goroutine, see:
	// https://github.com/Shopify/sarama/blob/main/consumer_group.go#L27-L29
	for {
		select {
		case message := <-claim.Messages():
			log.Printf("Message claimed: value = %s, timestamp = %v, topic = %s", string(message.Value), message.Timestamp, message.Topic)
			session.MarkMessage(message, "")

		// Should return when `session.Context()` is done.
		// If not, will raise `ErrRebalanceInProgress` or `read tcp <ip>:<port>: i/o timeout` when kafka rebalance. see:
		// https://github.com/Shopify/sarama/issues/1192
		case <-session.Context().Done():
			return nil
		}
	}
}
