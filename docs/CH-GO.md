// INSERT

package main

import (
	"context"
	"io"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
)

func main() {
	ctx := context.Background()

	conn, err := ch.Dial(ctx, ch.Options{})
	if err != nil {
		panic(err)
	}

	if err := conn.Do(ctx, ch.Query{
		Body: `CREATE TABLE IF NOT EXISTS test_table_insert
(
    ts                DateTime64(9),
    severity_text     Enum8('INFO'=1, 'DEBUG'=2),
    severity_number   UInt8,
	service_name      LowCardinality(String),
    body              String,
    name              String,
    arr               Array(String)
) ENGINE = Memory`,
	}); err != nil {
		panic(err)
	}

	// Define all columns of table.
	var (
		body      proto.ColStr
		name      proto.ColStr
		sevText   proto.ColEnum
		sevNumber proto.ColUInt8

		// or new(proto.ColStr).LowCardinality()
		serviceName = proto.NewLowCardinality(new(proto.ColStr))
		ts          = new(proto.ColDateTime64).WithPrecision(proto.PrecisionNano) // DateTime64(9)
		arr         = new(proto.ColStr).Array()                                   // Array(String)
		now         = time.Date(2010, 1, 1, 10, 22, 33, 345678, time.UTC)
	)

	// Append 10 rows to initial data block.
	for i := 0; i < 10; i++ {
		body.AppendBytes([]byte("Hello"))
		ts.Append(now)
		name.Append("name")
		sevText.Append("INFO")
		sevNumber.Append(10)
		arr.Append([]string{"foo", "bar", "baz"})
		serviceName.Append("service")
	}

	// Insert single data block.
	input := proto.Input{
		{Name: "ts", Data: ts},
		{Name: "severity_text", Data: &sevText},
		{Name: "severity_number", Data: &sevNumber},
		{Name: "service_name", Data: serviceName},
		{Name: "body", Data: &body},
		{Name: "name", Data: &name},
		{Name: "arr", Data: arr},
	}
	if err := conn.Do(ctx, ch.Query{
		Body: "INSERT INTO test_table_insert VALUES",
		// Or "INSERT INTO test_table_insert (ts, severity_text, severity_number, body, name, arr) VALUES"
		Input: input,
	}); err != nil {
		panic(err)
	}

	// Stream data to ClickHouse server in multiple data blocks.
	var blocks int
	if err := conn.Do(ctx, ch.Query{
		Body:  input.Into("test_table_insert"), // helper that generates INSERT INTO query with all columns
		Input: input,

		// OnInput is called to prepare Input data before encoding and sending
		// to ClickHouse server.
		OnInput: func(ctx context.Context) error {
			// On OnInput call, you should fill the input data.
			//
			// NB: You should reset the input columns, they are
			// not reset automatically.
			//
			// That is, we are re-using the same input columns and
			// if we will return nil without doing anything, data will be
			// just duplicated.

			input.Reset() // calls "Reset" on each column

			if blocks >= 10 {
				// Stop streaming.
				//
				// This will also write tailing input data if any,
				// but we just reset the input, so it is currently blank.
				return io.EOF
			}

			// Append new values:
			for i := 0; i < 10; i++ {
				body.AppendBytes([]byte("Hello"))
				ts.Append(now)
				name.Append("name")
				sevText.Append("DEBUG")
				sevNumber.Append(10)
				arr.Append([]string{"foo", "bar", "baz"})
				serviceName.Append("service")
			}

			// Data will be encoded and sent to ClickHouse server after returning nil.
			// The Do method will return error if any.
			blocks++
			return nil
		},
	}); err != nil {
		panic(err)
	}
}


// QUERY
Optimized Query Template
go
package main

import (
	"context"
	"fmt"
    "github.com/ClickHouse/ch-go"
    "github.com/ClickHouse/ch-go/proto"
)

func main() {
	ctx := context.Background()
	
	// 1. Initialize the low-level client
	client, err := ch.Dial(ctx, ch.Options{Address: "localhost:9000"})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	// 2. Pre-allocate column definitions ONCE to eliminate allocation loops
	var (
		colID        proto.ColInt64
		colName      proto.ColStr
		colTimestamp proto.ColDateTime
	)

	// Bind data variables to concrete schema definitions
	results := proto.Results{
		{Name: "id", Data: &colID},
		{Name: "name", Data: &colName},
		{Name: "timestamp", Data: &colTimestamp},
	}

	// 3. Construct the stream query
	q := ch.Query{
		Body:   "SELECT id, name, timestamp FROM target_db.events WHERE id > 0 LIMIT 100000",
		Result: results,
		
		// This callback executes as raw blocks streaming straight off the wire
		OnResult: func(ctx context.Context, b proto.Block) error {
			// b.Rows yields the number of elements in the current network data block
			for i := 0; i < b.Rows; i++ {
				// Access vectors directly using pure slice index mappings 
				// (No reflection, no type assertions, zero conversion overhead)
				id := colID.Row(i)
				name := colName.Row(i)
				ts := colTimestamp.Row(i)

				// Fast-path processing logic goes here
				_ = id
				_ = name
				_ = ts
			}
			
			// Returning nil tells ch-go to free the network buffer and request the next block
			return nil
		},
	}

	// 4. Fire the query pipeline
	if err := client.Do(ctx, q); err != nil {
		panic(err)
	}
}
Use code with caution.
