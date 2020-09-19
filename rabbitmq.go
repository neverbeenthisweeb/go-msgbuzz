package msgbuzz

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/streadway/amqp"
)

// RabbitMqClient RabbitMq implementation of MessageBus
type RabbitMqClient struct {
	conn         *amqp.Connection
	url          string
	consumerWg   sync.WaitGroup
	subscribers  []subscriber
	threadNum    int
	maxRcAttempt int
}

func NewRabbitMqClient(conn string, threadNum int, maxReconnectAttempt int) *RabbitMqClient {
	mc := &RabbitMqClient{
		url:          conn,
		threadNum:    threadNum,
		maxRcAttempt: maxReconnectAttempt,
	}
	mc.connectToBroker()
	return mc
}

func (m *RabbitMqClient) Publish(topicName string, body []byte) error {
	logger := logrus.WithField("method", "Publish")
	if m.conn == nil {
		panic("Tried to send message before connection was initialized. Don't do that.")
	}
	ch, err := m.conn.Channel() // Get a channel from the connection
	defer func() {
		errClose := ch.Close()
		if errClose != nil {
			logger.WithError(errClose).Warning("Error when closing channel")
		}
	}()
	if err != nil {
		return err
	}

	err = ch.ExchangeDeclare(
		topicName, // name of the exchange
		"fanout",  // type
		true,      // durable
		false,     // delete when complete
		false,     // internal
		false,     // noWait
		nil,       // arguments
	)
	failOnError(err, "Failed to register an Exchange")

	// Publishes a message onto the queue.
	err = ch.Publish(
		topicName, // exchange
		"",        // routing key
		false,     // mandatory
		false,     // immediate
		amqp.Publishing{
			ContentType: "application/json",
			Body:        body, // Our JSON body as []byte
		})
	logrus.Debugf("A message was sent to exchange %v: %v", topicName, string(body))
	return err
}

func (m *RabbitMqClient) On(topicName string, consumerName string, handlerFunc MessageHandler) error {
	m.subscribers = append(m.subscribers, subscriber{
		topicName:      topicName,
		consumerName:   consumerName,
		messageHandler: handlerFunc,
	})

	return nil
}

func (m *RabbitMqClient) Close() error {
	if m.conn == nil {
		return fmt.Errorf("trying to close closed connection")
	}
	if m.conn != nil {
		logrus.Infoln("Closing connection to AMQP broker")
		return m.conn.Close()
	}
	return nil
}

func (m *RabbitMqClient) StartConsuming() error {
	logger := logrus.WithField("method", "StartConsuming")

	if err := m.startConsumingSubscribers(); err != nil {
		return err
	}

	if m.maxRcAttempt > 1 {
		logger.Info("Adding consumerWg wait group due to reconnect possibility")
		m.consumerWg.Add(1)
	}

	m.consumerWg.Wait()

	logger.Info("Consumer is about to be closed")

	return nil
}

func (m *RabbitMqClient) startConsumingSubscribers() error {
	logger := logrus.WithField("method", "startConsumingSubscribers")
	logger.Infof("About to start consuming to %d subscribers", len(m.subscribers))

	for _, sub := range m.subscribers {
		for i := 0; i < m.threadNum; i++ {
			err := m.consume(sub.topicName, sub.consumerName, sub.messageHandler)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (m *RabbitMqClient) consume(topicName string, consumerName string, handlerFunc MessageHandler) error {
	ch, err := m.conn.Channel()
	failOnError(err, "Failed to open a channel")

	err = ch.Qos(1, 0, false)
	failOnError(err, "Failed setting qos prefetch to 1")

	logrus.Infof("About to subscribe to topic: %s", topicName)

	names := NewQueueNameGenerator(topicName, consumerName)
	// create dlx exchange and queue
	err = ch.ExchangeDeclare(names.DlxExchange(), "direct", true, false, false, false, nil)
	failOnError(err, "Failed declaring dlx exchange")
	_, err = ch.QueueDeclare(names.DlxQueue(), true, false, false, false, nil)
	failOnError(err, "Failed declaring dlx queue")
	err = ch.QueueBind(names.DlxQueue(), names.DlxQueue(), names.DlxExchange(), false, nil)
	failOnError(err, "Failed binding dlx exchange and queue")

	// create exchange for pub/sub
	err = ch.ExchangeDeclare(names.Exchange(), "fanout", true, false, false, false, nil)
	failOnError(err, "Failed declaring pub/sub exchange")
	// create dedicated queue for receiving message (create subscriber)
	_, err = ch.QueueDeclare(names.Queue(), true, false, false, false, amqp.Table{"x-dead-letter-exchange": names.DlxExchange(), "x-dead-letter-routing-key": names.DlxQueue()})
	failOnError(err, "Failed declaring pub/sub queue")
	// bind created queue with pub/sub exchange
	err = ch.QueueBind(names.Queue(), names.Queue(), names.Exchange(), false, nil)
	failOnError(err, "Failed binding pub/sub exchange and queue")

	// setup retry requeue exchange and binding
	err = ch.ExchangeDeclare(names.RetryExchange(), "direct", true, false, false, false, nil)
	failOnError(err, "Failed declaring retry exchange")
	err = ch.QueueBind(names.Queue(), names.Queue(), names.RetryExchange(), false, nil)
	failOnError(err, "Failed binding retry exchange and queue")
	// create retry queue
	_, err = ch.QueueDeclare(names.RetryQueue(), true, false, false, false, amqp.Table{"x-dead-letter-exchange": names.RetryExchange(), "x-dead-letter-routing-key": names.Queue()})
	failOnError(err, "Failed creating retry queue")

	deliveries, err := ch.Consume(
		names.Queue(), // queue
		"",            // consumer
		false,         // auto-ack
		false,         // exclusive
		false,         // no-local
		false,         // no-wait
		nil,           // args
	)
	failOnError(err, "Failed to register a consumer")

	m.consumerWg.Add(1)
	go consumeLoop(&m.consumerWg, ch, deliveries, handlerFunc, names)
	return nil
}

type subscriber struct {
	topicName      string
	consumerName   string
	messageHandler MessageHandler
}

func (m *RabbitMqClient) connectToBroker() error {
	logger := logrus.WithField("method", "connectToBroker")

	if m.url == "" {
		return errors.New("cannot initialize connection to broker, connection string not set")
	}

	var err error
	m.conn, err = amqp.Dial(fmt.Sprintf("%s/", m.url))
	if err != nil {
		return errors.New("failed to connect to AMQP compatible broker at: " + m.url + err.Error())
	}

	// spin notify close listener
	if m.maxRcAttempt > 0 {
		go func() {
			logger.Info("About to start notify close listener")
			notifyCloseErr := <-m.conn.NotifyClose(make(chan *amqp.Error))
			logger.WithError(notifyCloseErr).Warning("Connection is closed: Attempting to reconnect")
			if err := m.reconnect(); err != nil {
				m.consumerWg.Done()
			}
		}()
	}

	return nil
}

func (m *RabbitMqClient) reconnect() error {
	logger := logrus.WithField("method", "reconnect").WithField("url", m.url)
	logger.Infof("About to start reconnecting")

	// TODO: Make step configurable.
	step := 10 * time.Second

	for i := 1; i <= m.maxRcAttempt; i++ {
		// Sleep between attempts of reconnecting to avoid consecutive errors
		if i > 1 {
			time.Sleep(time.Duration(i-1) * step)
		}

		logger.Infof("Attempting to reconnect [%d/%d]", i, m.maxRcAttempt)

		if err := m.connectToBroker(); err != nil {
			logger.WithError(err).Warning("Error when connecting to broker: Continue another attempt to reconnect")
			continue
		}

		if err := m.startConsumingSubscribers(); err != nil {
			logger.WithError(err).Warning("Error when starting to consume subscribers: Continue another attempt to reconnect")
			continue
		}

		logger.Infof("Succesfully reconnect after %d attempts", i)

		return nil
	}

	return errors.New("maximum number of reconnect is reached")
}

func consumeLoop(wg *sync.WaitGroup, channel *amqp.Channel, deliveries <-chan amqp.Delivery, handlerFunc MessageHandler, names *QueueNameGenerator) {
	defer wg.Done()
	for d := range deliveries {

		if messageExpired(d) {
			logrus.WithField("body", string(d.Body)).Info("Max retry reached dropping msg")
			err := channel.Nack(d.DeliveryTag, false, false)
			if err != nil {
				logrus.WithError(err).WithField("message", string(d.Body)).Warning("Failed Nacking expired message")
			}
			continue
		}

		msgConfirm := NewRabbitMqMessageConfirm(channel, &d, names, d.Body)
		err := handlerFunc(msgConfirm, d.Body)
		if err != nil {
			logrus.WithError(err).Warning("Exception when processing message")
		}
	}
}

func messageExpired(delivery amqp.Delivery) bool {
	logrus.WithField("headers", delivery.Headers).Info("Message Headers")
	if delivery.Headers == nil {
		return false
	}

	maxRetries, ok := delivery.Headers["x-max-retries"]
	if !ok {
		logrus.Debug("No x-max-retries")
		return false
	}

	maxRetriesStr, ok := maxRetries.(string)
	if !ok {
		logrus.
			WithField("x-max-retires", maxRetries).
			WithField("x-max-retires type", reflect.TypeOf(maxRetries).String()).
			Warning("x-max-retries not string")
		return false
	}
	maxRetriesInt64, err := strconv.ParseInt(maxRetriesStr, 10, 64)
	if err != nil {
		logrus.WithField("x-max-retries", maxRetriesStr).Warning("x-max-retires is not numeric")
		return false
	}

	totalFailed, err := getTotalFailed(delivery)
	if err != nil {
		return false
	}

	return totalFailed > maxRetriesInt64
}

func getTotalFailed(delivery amqp.Delivery) (int64, error) {
	deathHeader, ok := delivery.Headers["x-death"]
	if !ok {
		logrus.Debug("No x-death")
		return 0, nil
	}

	deathList, ok := deathHeader.([]interface{})
	if !ok {
		logrus.WithField("type", reflect.TypeOf(deathHeader).String()).Warning("x-death not []interface{}")
		return 0, fmt.Errorf("invalid x-death type: not []interface{}")
	}

	if len(deathList) == 0 {
		logrus.Warning("x-death empty list")
		return 0, nil
	}

	firstEntry, ok := deathList[0].(amqp.Table)
	if !ok {
		logrus.WithField("type", reflect.TypeOf(deathList[0]).String()).Warning("x-death[0] not type amqp.Table")
		return 0, fmt.Errorf("invalid x-death[0] type: not amqp.Table")
	}

	countInt64, ok := firstEntry["count"].(int64)
	if !ok {
		logrus.WithField("type", reflect.TypeOf(firstEntry["count"]).String()).Warning("x-death[0]['count'] not int64")
		return 0, fmt.Errorf("invalid x-death[0]['count'] type: not int64")
	}

	return countInt64, nil
}

func failOnError(err error, msg string) {
	if err != nil {
		logrus.Errorf("%s: %s", msg, err)
		panic(fmt.Sprintf("%s: %s", msg, err))
	}
}
