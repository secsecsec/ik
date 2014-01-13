package main

import (
	"flag"
	"os"
	"net"
	"bytes"
	"encoding/json"
	"reflect"
	"time"
	"log"
	"fmt"
	"strconv"
	"github.com/ugorji/go/codec"
)

type Record struct {
	Timestamp uint64
	Data map[string]interface{}
}

type IkBench struct {
	codec codec.MsgpackHandle
}

type IkBenchParams struct {
	Host string
	Simple bool
	NumberOfRecordsToSend int
	NumberOfRecordsSentAtOnce int
	Concurrency int
	Tag string
	Data map[string]interface{}
	MaxRetryCount int
}

func (ikb *IkBench) encodeEntrySingle(buf *bytes.Buffer, tag string, record Record) error {
	enc := codec.NewEncoder(buf, &ikb.codec)
	return enc.Encode([]interface{} { tag, record.Timestamp, record.Data })
}

func (ikb *IkBench) encodeEntryBulk(buf *bytes.Buffer, tag string, records []Record) error {
	enc := codec.NewEncoder(buf, &ikb.codec)
	return enc.Encode([]interface{} { tag, records })
}

func (ikb *IkBench) Send(conn net.Conn, params *IkBenchParams) error {
	time_ := time.Now().Unix()
	records := make([]Record, params.NumberOfRecordsSentAtOnce)
	for i := 0; i < params.NumberOfRecordsSentAtOnce; i += 1 {
		records[i] = Record { Timestamp: uint64(time_), Data: params.Data }
	}
	buf := bytes.Buffer {}
	if params.Simple {
		for _, record := range records {
			err := ikb.encodeEntrySingle(&buf, params.Tag, record)
			if err != nil {
				return err
			}
		}
	} else {
		err := ikb.encodeEntryBulk(&buf, params.Tag, records)
		if err != nil {
			return err
		}
	}
	_, err := buf.WriteTo(conn)
	return err
}

func (ikb *IkBench) Run(logger *log.Logger, params *IkBenchParams) {
	numberOfAttempts := params.NumberOfRecordsToSend / params.NumberOfRecordsSentAtOnce
	numberOfAttemptsPerProc := numberOfAttempts / params.Concurrency
	remainder := numberOfAttempts % params.Concurrency
	sync := make(chan int)
	for i := 0; i < params.Concurrency; i += 1 {
		r := 0
		if i < remainder {
			r = 1
		}
		go func(id int, attempts int) {
			retryCount := params.MaxRetryCount
			outer: for {
				conn, err := net.Dial("tcp", params.Host)
				if err != nil {
					log.Print(err.Error())
					retryCount -= 1
					if retryCount < 0 {
						log.Fatal("retry count exceeded") // FIXME
					}
					continue
				}
				defer conn.Close()
				for i := 0; i < attempts; i += 1 {
					for {
						err = ikb.Send(conn, params)
						if err != nil {
							err_, ok := err.(net.Error)
							if ok {
								if err_.Temporary() {
									continue
								}
								err = conn.Close()
								if err != nil {
									log.Print(err.Error())
								}
							}
							break outer
						}
						break
					}
				}
				break
			}
			sync <- id
		}(i, numberOfAttemptsPerProc + r)
	}
	for i := 0; i < params.Concurrency; i += 1 {
		<-sync
	}
}

func NewIkBench() *IkBench {
	codec_ := codec.MsgpackHandle {}
	codec_.MapType = reflect.TypeOf(map[string]interface{}(nil))
	codec_.RawToString = false
	codec_.StructToArray = true
	return &IkBench { codec: codec_ }
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: %s [-concurrent N] [-multi N] [-no-packed] [-host HOST] [-data JSON] tag count\n", os.Args[0])
	flag.PrintDefaults()
	os.Exit(255)
}

func exitWithMessage(message string, exitStatus int) {
	fmt.Fprintf(os.Stderr, "%s: %s\n", os.Args[0], message)
	os.Exit(exitStatus)
}

func exitWithError(err error, exitStatus int) {
	exitWithMessage(err.Error(), exitStatus)
}

func main() {
	var host string
	var simple bool
	var numberOfRecordsToSend int
	var numberOfRecordsSentAtOnce int
	var concurrency int
	var tag string
	var jsonString string
	flag.IntVar(&concurrency, "concurrent", 1, "number of goroutines")
	flag.IntVar(&numberOfRecordsSentAtOnce, "multi", 1, "send multiple records at once")
	flag.BoolVar(&simple, "no-packed", false, "don't use lazy deserialization optimize")
	flag.StringVar(&host, "host", "localhost:24224", "fluent host")
	flag.StringVar(&jsonString, "data", `{ "message": "test" }`, "data to send (in JSON)")
	flag.Parse()
	args := flag.Args()
	if len(args) < 2 {
		usage()
	}
	tag = args[0]
	numberOfRecordsToSend, err := strconv.Atoi(args[1])
	if err != nil {
		exitWithError(err, 255)
	}
	data := make(map[string]interface{})
	err = json.Unmarshal([]byte(jsonString), &data)
	if err != nil {
		exitWithError(err, 255)
	}
	if numberOfRecordsToSend % numberOfRecordsSentAtOnce != 0 {
		exitWithMessage("the value of 'count' must be a multiple of 'multi'", 255)
	}
	if numberOfRecordsToSend / numberOfRecordsSentAtOnce < concurrency {
		exitWithMessage("the value of 'concurrency' must be equal to or greater than the division of 'count' by 'multi'", 255)
	}
	ikb := NewIkBench()
	ikb.Run(
		&log.Logger {},
		&IkBenchParams {
			Host: host,
			Simple: simple,
			NumberOfRecordsToSend: numberOfRecordsToSend,
			NumberOfRecordsSentAtOnce: numberOfRecordsSentAtOnce,
			Concurrency: concurrency,
			Tag: tag,
			Data: data,
			MaxRetryCount: 5,
		},
	)
}