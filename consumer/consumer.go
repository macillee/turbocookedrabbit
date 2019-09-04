package consumer

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/houseofcat/turbocookedrabbit/models"
	"github.com/houseofcat/turbocookedrabbit/pools"
	"github.com/streadway/amqp"
)

// Consumer receives messages from a RabbitMQ location.
type Consumer struct {
	Config           *models.RabbitSeasoning
	ChannelPool      *pools.ChannelPool
	QueueName        string
	ConsumerName     string
	QOS              uint32
	errors           chan error
	messageGroup     *sync.WaitGroup
	messages         chan *models.Message
	consumeStop      chan bool
	stopImmediate    bool
	started          bool
	autoAck          bool
	exclusive        bool
	noLocal          bool
	noWait           bool
	args             map[string]interface{}
	qosCountOverride int
	qosSizeOverride  int
	conLock          *sync.Mutex
}

// NewConsumerFromConfig creates a new Consumer to receive messages from a specific queuename.
func NewConsumerFromConfig(
	consumerConfig *models.ConsumerConfig,
	channelPool *pools.ChannelPool) (*Consumer, error) {

	if channelPool == nil {
		return nil, errors.New("can't start a consumer without a channel pool")
	} else if !channelPool.Initialized {
		channelPool.Initialize()
	}

	if consumerConfig.MessageBuffer == 0 || consumerConfig.ErrorBuffer == 0 {
		return nil, errors.New("message and/or error buffer in config can't be 0")
	}

	return &Consumer{
		Config:           nil,
		ChannelPool:      channelPool,
		QueueName:        consumerConfig.QueueName,
		ConsumerName:     consumerConfig.ConsumerName,
		errors:           make(chan error, consumerConfig.ErrorBuffer),
		messageGroup:     &sync.WaitGroup{},
		messages:         make(chan *models.Message, consumerConfig.MessageBuffer),
		consumeStop:      make(chan bool, 1),
		stopImmediate:    false,
		started:          false,
		autoAck:          consumerConfig.AutoAck,
		exclusive:        consumerConfig.Exclusive,
		noWait:           consumerConfig.NoWait,
		args:             consumerConfig.Args,
		qosCountOverride: consumerConfig.QosCountOverride,
		qosSizeOverride:  consumerConfig.QosSizeOverride,
		conLock:          &sync.Mutex{},
	}, nil
}

// NewConsumer creates a new Consumer to receive messages from a specific queuename.
func NewConsumer(
	config *models.RabbitSeasoning,
	channelPool *pools.ChannelPool,
	queuename string,
	consumerName string,
	autoAck bool,
	exclusive bool,
	noWait bool,
	args map[string]interface{},
	qosCountOverride int, // if zero ignored
	qosSizeOverride int, // if zero ignored
	messageBuffer uint32,
	errorBuffer uint32) (*Consumer, error) {

	var err error
	if channelPool == nil {
		channelPool, err = pools.NewChannelPool(config, nil, true)
		if err != nil {
			return nil, err
		}
	}

	if messageBuffer == 0 || errorBuffer == 0 {
		return nil, errors.New("message and/or error buffer can't be 0")
	}

	return &Consumer{
		Config:           config,
		ChannelPool:      channelPool,
		QueueName:        queuename,
		ConsumerName:     consumerName,
		errors:           make(chan error, errorBuffer),
		messageGroup:     &sync.WaitGroup{},
		messages:         make(chan *models.Message, messageBuffer),
		consumeStop:      make(chan bool, 1),
		stopImmediate:    false,
		started:          false,
		autoAck:          autoAck,
		exclusive:        exclusive,
		noWait:           noWait,
		args:             args,
		qosCountOverride: qosCountOverride,
		qosSizeOverride:  qosSizeOverride,
		conLock:          &sync.Mutex{},
	}, nil
}

// StartConsuming starts the Consumer.
func (con *Consumer) StartConsuming() error {
	con.conLock.Lock()
	defer con.conLock.Unlock()

	if con.started {
		return errors.New("can't start an already started consumer")
	}

	con.FlushErrors()
	con.FlushStop()

	go con.startConsuming()
	con.started = true
	return nil
}

func (con *Consumer) startConsuming() {

GetChannelLoop:
	for {
		// Detect if we should stop.
		select {
		case stop := <-con.consumeStop:
			if stop {
				break GetChannelLoop
			}
		default:
			break
		}

		// Get Channel
		chanHost, err := con.ChannelPool.GetChannel()
		if err != nil {
			go func() { con.errors <- err }()
			time.Sleep(1 * time.Second)
			continue // Retry
		}

		// Quality of Service channel overrides
		if con.qosCountOverride != 0 && con.qosSizeOverride != 0 {
			chanHost.Channel.Qos(con.qosCountOverride, con.qosSizeOverride, false)
		}

		// Start Consuming
		deliveryChan, err := chanHost.Channel.Consume(con.QueueName, con.ConsumerName, con.autoAck, con.exclusive, false, con.noWait, con.args)
		if err != nil {
			go func() { con.errors <- err }()
			time.Sleep(1 * time.Second)
			continue // Retry
		}

	GetDeliveriesLoop:
		for {
			// Listen for channel closure (close errors).
			// Highest priority so separated to it's own select.
			select {
			case amqpError := <-chanHost.CloseErrors():
				if amqpError != nil {
					go func() {
						con.errors <- fmt.Errorf("consumer's current channel closed\r\n[reason: %s]\r\n[code: %d]", amqpError.Reason, amqpError.Code)
					}()

					break GetDeliveriesLoop
				}
			default:
				break
			}

			// Convert amqp.Delivery into our internal struct for later use.
			select {
			case delivery := <-deliveryChan: // all buffered deliveries are wipe on a channel close error
				con.messageGroup.Add(1)
				con.convertDelivery(chanHost.Channel, &delivery, !con.autoAck)
			default:
				break
			}

			// Detect if we should stop.
			select {
			case stop := <-con.consumeStop:
				if stop {
					break GetChannelLoop
				}
			default:
				break
			}
		}

		// Quality of Service channel overrides reset
		if con.Config.PoolConfig.GlobalQosCount != 0 && con.Config.PoolConfig.GlobalQosSize != 0 {
			chanHost.Channel.Qos(
				con.Config.PoolConfig.GlobalQosCount,
				con.Config.PoolConfig.GlobalQosCount,
				false)
		}
	}

	con.conLock.Lock()
	immediateStop := con.stopImmediate
	con.conLock.Unlock()

	if !immediateStop {
		con.messageGroup.Wait() // wait for every message to be received to the internal queue
	}

	con.conLock.Lock()
	con.started = false
	con.stopImmediate = false
	con.conLock.Unlock()
}

// StopConsuming allows you to signal stop to the consumer.
// Will stop on the consumer channelclose or responding to signal after getting all remaining deviveries.
func (con *Consumer) StopConsuming(immediate bool) error {
	con.conLock.Lock()
	defer con.conLock.Unlock()

	if !con.started {
		return errors.New("can't stop a stopped consumer")
	}

	con.stopImmediate = true

	go func() { con.consumeStop <- true }()
	return nil
}

// Messages yields all the internal messages ready for consuming.
func (con *Consumer) Messages() <-chan *models.Message {
	return con.messages
}

// Errors yields all the internal errs for consuming messages.
func (con *Consumer) Errors() <-chan error {
	return con.errors
}

func (con *Consumer) convertDelivery(amqpChan *amqp.Channel, delivery *amqp.Delivery, isAckable bool) {
	msg := models.NewMessage(
		isAckable,
		delivery.Body,
		delivery.DeliveryTag,
		amqpChan,
	)

	go func() {
		defer con.messageGroup.Done() // finished after getting the message in the channel
		con.messages <- msg
	}()
}

// FlushStop allows you to flush out all previous Stop signals.
func (con *Consumer) FlushStop() {

FlushLoop:
	for {
		select {
		case <-con.consumeStop:
			break
		default:
			break FlushLoop
		}
	}
}

// FlushErrors allows you to flush out all previous Errors.
func (con *Consumer) FlushErrors() {

FlushLoop:
	for {
		select {
		case <-con.errors:
			break
		default:
			break FlushLoop
		}
	}
}

// FlushMessages allows you to flush out all previous Messages.
// WARNING: THIS WILL RESULT IN LOST MESSAGES.
func (con *Consumer) FlushMessages() {

FlushLoop:
	for {
		select {
		case <-con.messages:
		default:
			break FlushLoop
		}
	}
}
