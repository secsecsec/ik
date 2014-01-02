package plugins

import (
	"github.com/moriyoshi/ik"
	"errors"
	"fmt"
	"github.com/ugorji/go/codec"
	"io"
	"log"
	"net"
	"reflect"
	"strconv"
	"sync/atomic"
)

type forwardClient struct {
	input  *ForwardInput
	logger *log.Logger
	conn   net.Conn
	codec  *codec.MsgpackHandle
	enc    *codec.Encoder
	dec    *codec.Decoder
}

type ForwardInput struct {
	factory  *ForwardInputFactory
	port     ik.Port
	logger   *log.Logger
	bind     string
	listener net.Listener
	codec    *codec.MsgpackHandle
	clients  map[net.Conn]*forwardClient
	entries  int64
}

type EntryCountTopic struct {
	input *ForwardInput
}

type ConnectionCountTopic struct {
	input *ForwardInput
}

type ForwardInputFactory struct {
}

func coerceInPlace(data map[string]interface{}) {
	for k, v := range data {
		switch v_ := v.(type) {
		case []byte:
			data[k] = string(v_) // XXX: byte => rune
		case map[string]interface{}:
			coerceInPlace(v_)
		}
	}
}

func decodeTinyEntries(tag []byte, entries []interface{}) ([]ik.FluentRecord, error) {
	retval := make([]ik.FluentRecord, len(entries))
	for i, _entry := range entries {
		entry := _entry.([]interface{})
		timestamp, ok := entry[0].(uint64)
		if !ok {
			return nil, errors.New("Failed to decode timestamp field")
		}
		data, ok := entry[1].(map[string]interface{})
		if !ok {
			return nil, errors.New("Failed to decode data field")
		}
		coerceInPlace(data)
		retval[i] = ik.FluentRecord{
			Tag:       string(tag), // XXX: byte => rune
			Timestamp: timestamp,
			Data:      data,
		}
	}
	return retval, nil
}

func (c *forwardClient) decodeEntries() ([]ik.FluentRecord, error) {
	v := []interface{}{nil, nil, nil}
	err := c.dec.Decode(&v)
	if err != nil {
		return nil, err
	}
	tag, ok := v[0].([]byte)
	if !ok {
		return nil, errors.New("Failed to decode tag field")
	}

	var retval []ik.FluentRecord
	switch timestamp_or_entries := v[1].(type) {
	case uint64:
		timestamp := timestamp_or_entries
		data, ok := v[2].(map[string]interface{})
		if !ok {
			return nil, errors.New("Failed to decode data field")
		}
		coerceInPlace(data)
		retval = []ik.FluentRecord{
			{
				Tag:       string(tag), // XXX: byte => rune
				Timestamp: timestamp,
				Data:      data,
			},
		}
	case float64:
		timestamp := uint64(timestamp_or_entries)
		data, ok := v[2].(map[string]interface{})
		if !ok {
			return nil, errors.New("Failed to decode data field")
		}
		retval = []ik.FluentRecord{
			{
				Tag:       string(tag), // XXX: byte => rune
				Timestamp: timestamp,
				Data:      data,
			},
		}
	case []interface{}:
		if !ok {
			return nil, errors.New("Unexpected payload format")
		}
		retval, err = decodeTinyEntries(tag, timestamp_or_entries)
		if err != nil {
			return nil, err
		}
	case []byte:
		entries := make([]interface{}, 0)
		err := codec.NewDecoderBytes(timestamp_or_entries, c.codec).Decode(&entries)
		if err != nil {
			return nil, err
		}
		retval, err = decodeTinyEntries(tag, entries)
		if err != nil {
			return nil, err
		}
	default:
		return nil, errors.New(fmt.Sprintf("Unknown type: %t", timestamp_or_entries))
	}
	atomic.AddInt64(&c.input.entries, int64(len(retval)))
	return retval, nil
}

func (c *forwardClient) handle() {
	for {
		entries, err := c.decodeEntries()
		if err != nil {
			err_, ok := err.(net.Error)
			if ok {
				if err_.Temporary() {
					c.logger.Println("Temporary failure: %s", err_.Error())
					continue
				}
			}
			if err == io.EOF {
				c.logger.Printf("Client %s closed the connection", c.conn.RemoteAddr().String())
			} else {
				c.logger.Print(err.Error())
			}
			break
		}
		c.input.Port().Emit(entries)
	}
	err := c.conn.Close()
	if err != nil {
		c.logger.Print(err.Error())
	}
	c.input.markDischarged(c)
}

func newForwardClient(input *ForwardInput, logger *log.Logger, conn net.Conn, _codec *codec.MsgpackHandle) *forwardClient {
	c := &forwardClient{
		input:  input,
		logger: logger,
		conn:   conn,
		codec:  _codec,
		enc:    codec.NewEncoder(conn, _codec),
		dec:    codec.NewDecoder(conn, _codec),
	}
	input.markCharged(c)
	return c
}

func (input *ForwardInput) Factory() ik.InputFactory {
	return input.factory
}

func (input *ForwardInput) Port() ik.Port {
	return input.port
}

func (input *ForwardInput) Run() error {
	conn, err := input.listener.Accept()
	if err != nil {
		input.logger.Print(err.Error())
		return err
	}
	go newForwardClient(input, input.logger, conn, input.codec).handle()
	return ik.Continue
}

func (input *ForwardInput) Shutdown() error {
	for conn, _ := range input.clients {
		err := conn.Close()
		if err != nil {
			input.logger.Printf("Error during closing connection: %s", err.Error())
		}
	}
	return input.listener.Close()
}

func (input *ForwardInput) Dispose() {
	input.Shutdown()
}

func (input *ForwardInput) markCharged(c *forwardClient) {
	input.clients[c.conn] = c
}

func (input *ForwardInput) markDischarged(c *forwardClient) {
	delete(input.clients, c.conn)
}

func newForwardInput(factory *ForwardInputFactory, logger *log.Logger, engine ik.Engine, bind string, port ik.Port) (*ForwardInput, error) {
	_codec := codec.MsgpackHandle{}
	_codec.MapType = reflect.TypeOf(map[string]interface{}(nil))
	_codec.RawToString = false
	listener, err := net.Listen("tcp", bind)
	if err != nil {
		logger.Print(err.Error())
		return nil, err
	}
	retval := &ForwardInput{
		factory:  factory,
		port:     port,
		logger:   logger,
		bind:     bind,
		listener: listener,
		codec:    &_codec,
		clients:  make(map[net.Conn]*forwardClient),
		entries:  0,
	}
	engine.Scorekeeper().AddTopic(ik.ScorekeeperTopic {
		Plugin: factory,
		Name: "entries",
		DisplayName: "Total number of entries",
		Description: "Total number of entries received so far",
		Fetcher: &EntryCountTopic { retval },
	})
	engine.Scorekeeper().AddTopic(ik.ScorekeeperTopic {
		Plugin: factory,
		Name: "connections",
		DisplayName: "Connections",
		Description: "Number of connections currently handled",
		Fetcher: &ConnectionCountTopic { retval },
	})
	return retval, nil
}

func (factory *ForwardInputFactory) Name() string {
	return "forward"
}

func (factory *ForwardInputFactory) New(engine ik.Engine, config *ik.ConfigElement) (ik.Input, error) {
	listen, ok := config.Attrs["listen"]
	if !ok {
		listen = ""
	}
	netPort, ok := config.Attrs["port"]
	if !ok {
		netPort = "24224"
	}
	bind := listen + ":" + netPort
	return newForwardInput(factory, engine.Logger(), engine, bind, engine.DefaultPort())
}

func (topic *EntryCountTopic) Markup() (ik.Markup, error) {
	text, err := topic.PlainText()
	if err != nil {
		return ik.Markup {}, err
	}
	return ik.Markup { []ik.MarkupChunk { { Text: text } } }, nil
}

func (topic *EntryCountTopic) PlainText() (string, error) {
	return strconv.FormatInt(topic.input.entries, 10), nil
}

func (topic *ConnectionCountTopic) Markup() (ik.Markup, error) {
	text, err := topic.PlainText()
	if err != nil {
		return ik.Markup {}, err
	}
	return ik.Markup { []ik.MarkupChunk { { Text: text } } }, nil
}

func (topic *ConnectionCountTopic) PlainText() (string, error) {
	return strconv.Itoa(len(topic.input.clients)), nil // XXX: race
}

var _ = AddPlugin(&ForwardInputFactory{})
