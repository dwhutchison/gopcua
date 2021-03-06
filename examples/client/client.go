// Copyright 2018 gopcua authors. All rights reserved.
// Use of this source code is governed by a MIT-style license that can be
// found in the LICENSE file.

/*
Command client provides a connection establishment of OPC UA Secure Conversation.

XXX - Currently this command just initiates the connection(UACP) to the specified endpoint.
*/
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"log"

	"github.com/wmnsk/gopcua/uacp"
	"github.com/wmnsk/gopcua/uasc"
	"github.com/wmnsk/gopcua/utils"
)

func main() {
	var (
		endpoint   = flag.String("endpoint", "opc.tcp://example.com/foo/bar", "OPC UA Endpoint URL")
		payloadHex = flag.String("payload", "deadbeef", "Payload to send in hex stream format")
	)
	flag.Parse()

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	conn, err := uacp.Dial(ctx, *endpoint)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	log.Printf("Successfully established connection with %v", conn.RemoteEndpoint())

	uascConfig := uasc.NewConfig(
		1, "http://opcfoundation.org/UA/SecurityPolicy#None", nil, nil, 0, 0,
	)
	secChan, err := uasc.OpenSecureChannel(ctx, conn, uascConfig, 1, 1, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := secChan.Close(); err != nil {
			log.Println("Failed to close secure channel")
		}
		log.Println("Successfully closed secure channel")
	}()
	log.Printf("Successfully opened secure channel with %v", conn.RemoteEndpoint())

	payload, err := hex.DecodeString(*payloadHex)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := secChan.WriteService(payload); err != nil {
		log.Fatal(err)
	}
	log.Printf("Successfully sent message: %x\n%s", payload, utils.Wireshark(0, payload))
}
