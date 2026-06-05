//go:build ignore
// +build ignore

package experiment

import (
	"context"
	"io"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
)

type DirectExchangeOrderFilledInserter struct {
	conn  *ch.Client
	query string

	colBlock      proto.ColUInt64
	colTime       proto.ColDateTime64
	colTxIdx      proto.ColUInt64
	colLogIdx     proto.ColUInt64
	colMaker      proto.ColFixedStr
	colTaker      proto.ColFixedStr
	colMakerAsset proto.ColUInt256
	colTakerAsset proto.ColUInt256
	colMakerAmt   proto.ColUInt256
	colTakerAmt   proto.ColUInt256
	colFee        proto.ColUInt256

	inputCols []proto.InputColumn
}

func NewDirectExchangeOrderFilledInserter(conn *ch.Client, db string) *DirectExchangeOrderFilledInserter {
	in := &DirectExchangeOrderFilledInserter{
		conn:  conn,
		query: "INSERT INTO " + db + ".exchange_order_filled_events (block_number, block_timestamp, transaction_index, log_index, maker, taker, makerAssetId, takerAssetId, makerAmountFilled, takerAmountFilled, fee) VALUES",
	}

	in.colMaker = proto.ColFixedStr{Size: 20}
	in.colTaker = proto.ColFixedStr{Size: 20}

	in.colTime.WithPrecision(proto.Precision(3))
	in.colTime.WithLocation(time.UTC)

	in.inputCols = []proto.InputColumn{
		{Name: "block_number", Data: &in.colBlock},
		{Name: "block_timestamp", Data: &in.colTime},
		{Name: "transaction_index", Data: &in.colTxIdx},
		{Name: "log_index", Data: &in.colLogIdx},
		{Name: "maker", Data: &in.colMaker},
		{Name: "taker", Data: &in.colTaker},
		{Name: "makerAssetId", Data: &in.colMakerAsset},
		{Name: "takerAssetId", Data: &in.colTakerAsset},
		{Name: "makerAmountFilled", Data: &in.colMakerAmt},
		{Name: "takerAmountFilled", Data: &in.colTakerAmt},
		{Name: "fee", Data: &in.colFee},
	}

	return in
}

func (in *DirectExchangeOrderFilledInserter) Insert(ctx context.Context, events []generated.ExchangeOrderFilled) error {
	if len(events) == 0 {
		return nil
	}

	total := len(events)
	processed := 0
	chunkSize := 10000

	return in.conn.Do(ctx, ch.Query{
		Body:  in.query,
		Input: in.inputCols,
		OnInput: func(ctx context.Context) error {
			in.colBlock.Reset()
			in.colTime.Reset()
			in.colTxIdx.Reset()
			in.colLogIdx.Reset()
			in.colMaker.Reset()
			in.colTaker.Reset()
			in.colMakerAsset.Reset()
			in.colTakerAsset.Reset()
			in.colMakerAmt.Reset()
			in.colTakerAmt.Reset()
			in.colFee.Reset()

			if processed >= total {
				return io.EOF
			}

			end := processed + chunkSize
			if end > total {
				end = total
			}

			for i := processed; i < end; i++ {
				ev := &events[i]
				in.colBlock.Append(ev.BlockNumber)
				in.colTime.Append(ev.BlockTimestamp)
				in.colTxIdx.Append(ev.TransactionIndex)
				in.colLogIdx.Append(ev.LogIndex)
				in.colMaker.Append(ev.Maker[:])
				in.colTaker.Append(ev.Taker[:])
				in.colMakerAsset.Append(proto.UInt256{
					Low:  proto.UInt128{Low: ev.MakerAssetID[0], High: ev.MakerAssetID[1]},
					High: proto.UInt128{Low: ev.MakerAssetID[2], High: ev.MakerAssetID[3]},
				})
				in.colTakerAsset.Append(proto.UInt256{
					Low:  proto.UInt128{Low: ev.TakerAssetID[0], High: ev.TakerAssetID[1]},
					High: proto.UInt128{Low: ev.TakerAssetID[2], High: ev.TakerAssetID[3]},
				})
				in.colMakerAmt.Append(proto.UInt256{
					Low:  proto.UInt128{Low: ev.MakerAmountFilled[0], High: ev.MakerAmountFilled[1]},
					High: proto.UInt128{Low: ev.MakerAmountFilled[2], High: ev.MakerAmountFilled[3]},
				})
				in.colTakerAmt.Append(proto.UInt256{
					Low:  proto.UInt128{Low: ev.TakerAmountFilled[0], High: ev.TakerAmountFilled[1]},
					High: proto.UInt128{Low: ev.TakerAmountFilled[2], High: ev.TakerAmountFilled[3]},
				})
				in.colFee.Append(proto.UInt256{
					Low:  proto.UInt128{Low: ev.Fee[0], High: ev.Fee[1]},
					High: proto.UInt128{Low: ev.Fee[2], High: ev.Fee[3]},
				})
			}
			processed = end
			return nil
		},
	})
}
