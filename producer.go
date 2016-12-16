package kinetic

import (
	"errors"
	"log"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"sync/atomic"

	gokinesis "github.com/rewardStyle/go-kinesis"
)

const (
	firehoseURL     = "https://kinesis.%s.amazonaws.com"
	firehoseVersion = "20150804"

	kinesisType = iota
	firehoseType
)

var (
	ThroughputExceededError = errors.New("Configured AWS Kinesis throughput has been exceeded!")
	KinesisFailureError     = errors.New("AWS Kinesis internal failure.")
	BadConcurrencyError     = errors.New("Concurrency must be greater than zero.")
	DroppedMessageError     = errors.New("Channel is full, dropped message.")
)

// Producer keeps a queue of messages on a channel and continually attempts
// to POST the records using the PutRecords method. If the messages were
// not sent successfully they are placed back on the queue to retry
type Producer struct {
	*kinesis

	producerType int

	concurrency   int
	concurrencyMu sync.Mutex
	sem           chan bool

	wg sync.WaitGroup

	producing   bool
	producingMu sync.Mutex
	typeMu      sync.Mutex

	errors chan error

	// We need to ensure that the sent messages were successfully processed
	// before removing them from this local queue
	messages   chan *Message
	interrupts chan os.Signal

	stats Stats
}

type Stats struct {
	incCount     int64
	processCount int64
	retryCount   int64
	succCount    int64
	errCount     int64
	dropCount    int64
}

func (p *Producer) init(stream, shard, shardIterType, accessKey, secretKey, region string, concurrency int) (*Producer, error) {
	var err error
	if concurrency < 1 {
		return nil, BadConcurrencyError
	}
	if stream == "" {
		return nil, NullStreamError
	}

	p.setConcurrency(concurrency)
	p.setProducerType(kinesisType)
	p.stats = Stats{}

	p.initChannels()

	p.kinesis, err = new(kinesis).init(stream, shard, shardIterType, accessKey, secretKey, region)
	if err != nil {
		return p, err
	}

	return p.activate()
}

func (p *Producer) initChannels() {
	p.sem = make(chan bool, p.getConcurrency())
	p.errors = make(chan error, p.getConcurrency())
	p.messages = make(chan *Message, p.msgBufSize())

	p.interrupts = make(chan os.Signal, 1)
	signal.Notify(p.interrupts, os.Interrupt)
}

func (p *Producer) setConcurrency(concurrency int) {
	p.concurrencyMu.Lock()
	p.concurrency = concurrency
	p.concurrencyMu.Unlock()
}

func (p *Producer) getConcurrency() int {
	p.concurrencyMu.Lock()
	defer p.concurrencyMu.Unlock()
	return p.concurrency
}

func (p *Producer) msgBufSize() int {
	p.concurrencyMu.Lock()
	defer p.concurrencyMu.Unlock()
	return p.concurrency * 1000
}

func (p *Producer) setProducerType(producerType int) {
	p.typeMu.Lock()
	p.producerType = producerType
	p.typeMu.Unlock()
}

func (p *Producer) getProducerType() int {
	p.typeMu.Lock()
	defer p.typeMu.Unlock()
	return p.producerType
}

func (p *Producer) activate() (*Producer, error) {
	// Is the stream ready?
	var active bool
	var err error

	if p.getProducerType() == kinesisType {
		active, err = p.checkActive()
	} else {
		active, err = p.checkFirehoseActive()
	}

	if err != nil || active != true {
		if err != nil {
			return p, err
		} else {
			return p, NotActiveError
		}
	}

	// go start feeder consumer and let listen processes them
	go p.produce()

	return p, err
}

// Initialize a producer with the params specified in the configuration file
func (p *Producer) Init() (*Producer, error) {
	return p.init(conf.Kinesis.Stream, conf.Kinesis.Shard, ShardIterTypes[conf.Kinesis.ShardIteratorType], conf.AWS.AccessKey, conf.AWS.SecretKey, conf.AWS.Region, conf.Concurrency.Producer)
}

// Initialize a producer with the specified configuration: stream, shard, shard-iter-type, access-key, secret-key, and region
func (p *Producer) InitC(stream, shard, shardIterType, accessKey, secretKey, region string, concurrency int) (*Producer, error) {
	return p.init(stream, shard, shardIterType, accessKey, secretKey, region, concurrency)
}

// Re-initialize kinesis client with new endpoint. Used for testing with kinesalite
func (p *Producer) NewEndpoint(endpoint, stream string) {
	// Re-initialize kinesis client for testing
	p.kinesis.client = p.kinesis.newClient(endpoint, stream)

	if !p.IsProducing() {
		go p.produce()
	}
}

// Each shard can support up to 1,000 records per second for writes, up to a maximum total
// data write rate of 1 MB per second (including partition keys). This write limit applies
// to operations such as PutRecord and PutRecords.
// TODO: payload inspection & throttling
// http://docs.aws.amazon.com/kinesis/latest/dev/service-sizes-and-limits.html
func (p *Producer) kinesisFlush(counter *int, timer *time.Time) bool {
	// If a second has passed since the last timer start, reset the timer
	if time.Now().After(timer.Add(1 * time.Second)) {
		*timer = time.Now()
		*counter = 0
	}

	*counter++

	// If we have attempted 1000 times and it has been less than one second
	// since we started sending then we need to wait for the second to finish
	if *counter >= kinesisWritesPerSec && !(time.Now().After(timer.Add(1 * time.Second))) {
		// Wait for the remainder of the second - timer and counter
		// will be reset on next pass
		<-time.After(1*time.Second - time.Since(*timer))
	}

	return true
}

func (p *Producer) setProducing(producing bool) {
	p.producingMu.Lock()
	p.producing = producing
	p.producingMu.Unlock()

}

// Identifies whether or not the messages are queued for POSTing to the stream
func (p *Producer) IsProducing() bool {
	p.producingMu.Lock()
	defer p.producingMu.Unlock()
	return p.producing
}

// http://docs.aws.amazon.com/kinesis/latest/APIReference/API_PutRecords.html
//
// Maximum of 1000 requests a second for a single shard. Each PutRecords can
// accept a maximum of 500 records per request and each record can be as large
// as 1MB per record OR 5MB per request
func (p *Producer) produce() {
	p.setProducing(true)

	counter := 0
	timer := time.Now()

stop:
	for {
		getLock(p.sem)

		select {
		case msg := <-p.Messages():
			p.incMsgCount()
			atomic.AddInt64(&p.stats.processCount, 1)

			if conf.Debug.Verbose && p.getMsgCount()%100 == 0 {
				log.Println("Received message to send. Total messages received: " + strconv.FormatInt(p.getMsgCount(), 10))
			}

			// TODO interface out kinesis / firehose as common client interface
			kargs := p.args()
			fargs := p.firehoseArgs()

			if p.getProducerType() == kinesisType {
				kargs.AddRecord(msg.Value(), string(msg.Key()))
			} else if p.getProducerType() == firehoseType {
				fargs.AddRecord(msg.Value(), string(msg.Key()))
			}

			if p.getProducerType() == firehoseType && p.firehoseFlush(&counter, &timer) {
				p.wg.Add(1)
				go func() {
					p.sendFirehoseRecords(fargs)
					p.wg.Done()
				}()
			} else if p.kinesisFlush(&counter, &timer) {
				p.wg.Add(1)
				go func() {
					p.sendRecords(kargs)
					p.wg.Done()
				}()
			}

			// TODO releasing the semaphore when the sendRecords above are async
			<-p.sem

			break
		case sig := <-p.interrupts:
			go p.handleInterrupt(sig)
			break stop
		case err := <-p.Errors():
			// TODO these errors only increment when channel is used which is only in verbose mode
			p.incErrCount()
			p.wg.Add(1)
			go func() {
				p.handleError(err)
				p.wg.Done()
			}()

			// TODO moved semaphore release out of handleError to here where it was acquired
			// was originally deferred after wg.Done()
			<-p.sem
		}
	}

	p.setProducing(false)
}

func (p *Producer) Messages() <-chan *Message {
	return p.messages
}

func (p *Producer) Errors() <-chan error {
	return p.errors
}

// TODO combine Send and TryToSend to have a common send internal func they use

// Send a message to the queue for POSTing
func (p *Producer) Send(msg *Message) {
	atomic.AddInt64(&p.stats.incCount, 1)
	p.send(msg)
}

func (p *Producer) send(msg *Message) {
	// Add the terminating record indicator
	p.setTerminatingIndicator(msg)

	p.wg.Add(1)
	go func() {
		p.messages <- msg
		p.wg.Done()
	}()
}

// TryToSend tries to send the message, but if the channel is full it drops the message, and returns an error.
func (p *Producer) TryToSend(msg *Message) error {
	atomic.AddInt64(&p.stats.incCount, 1)

	// Add the terminating record indicator
	p.setTerminatingIndicator(msg)

	select {
	case p.messages <- msg:
		return nil
	default:
		atomic.AddInt64(&p.stats.dropCount, 1)
		return DroppedMessageError
	}
}

func (p *Producer) setTerminatingIndicator(msg *Message) {
	if p.getProducerType() == firehoseType {
		msg.SetValue(append(msg.Value(), truncatedRecordTerminator...))
	}
}

// If our payload is larger than allowed Kinesis will write as much as
// possible and fail the rest. We can then put them back on the queue
// to re-send
func (p *Producer) sendRecords(args *gokinesis.RequestArgs) {
	if p.getProducerType() != kinesisType {
		return
	}

	putResp, err := p.client.PutRecords(args)
	// TODO we populate error channel here and then also below; This just used for general errors
	if err != nil && conf.Debug.Verbose {
		p.errors <- err
	}

	if putResp != nil {
		// Because we do not know which of the records was successful or failed
		// we need to put them all back on the queue
		if putResp.FailedRecordCount > 0 {
			if conf.Debug.Verbose {
				log.Println("Failed records: " + strconv.Itoa(putResp.FailedRecordCount))
			}

			for idx, resp := range putResp.Records {
				// Put failed records back on the queue
				if resp.ErrorCode != "" || resp.ErrorMessage != "" {
					p.decMsgCount()
					p.errors <- errors.New(resp.ErrorMessage)

					atomic.AddInt64(&p.stats.errCount, 1)

					p.retryRecord(args.Records[idx])

					if conf.Debug.Verbose {
						log.Println("Message in failed PutRecords put back on the queue: " + string(args.Records[idx].Data))
					}
				} else {
					// Messages were successful as they do not have error message or code
					atomic.AddInt64(&p.stats.succCount, 1)
				}
			}
		} else {
			// All records were successful
			atomic.AddInt64(&p.stats.succCount, int64(len(putResp.Records)))
		}
	} else if putResp == nil {
		// Assume all records were error
		atomic.AddInt64(&p.stats.errCount, int64(len(args.Records)))
		// Retry posting these records as they most likely were not posted successfully
		p.retryRecords(args.Records)
	}

	if conf.Debug.Verbose && p.getMsgCount()%100 == 0 {
		log.Println("Messages sent so far: " + strconv.FormatInt(p.getMsgCount(), 10))
	}
}

func (p *Producer) retryRecords(records []gokinesis.Record) {
	for _, record := range records {
		p.retryRecord(record)

		if conf.Debug.Verbose {
			log.Println("Message in nil send response put back on the queue: " + string(record.Data))
		}
	}
}

func (p *Producer) retryRecord(record gokinesis.Record) {
	atomic.AddInt64(&p.stats.retryCount, 1)
	p.send(new(Message).Init(record.Data, record.PartitionKey))
}

// Stops queuing and producing and waits for all tasks to finish
func (p *Producer) Close() error {
	if conf.Debug.Verbose {
		log.Println("Producer is waiting for all tasks to finish...")
	}

	p.wg.Wait()

	// Stop producing
	go func() {
		p.interrupts <- syscall.SIGINT
	}()

	if conf.Debug.Verbose {
		log.Println("Producer is shutting down.")
	}

	return nil
}

func (p *Producer) handleInterrupt(signal os.Signal) {
	if conf.Debug.Verbose {
		log.Println("Producer received interrupt signal")
	}

	defer func() {
		<-p.sem
	}()

	p.Close()
}

func (p *Producer) handleError(err error) {
	if err != nil && conf.Debug.Verbose {
		log.Println("Received error: ", err.Error())
	}
}

// GetStats will return copy of current stats
func (p *Producer) GetStats() Stats {
	return p.stats
}
