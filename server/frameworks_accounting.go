package server

import (
	"math"
	"math/big"

	obs "github.com/Livepeer-FrameWorks/monorepo/pkg/networkobservation"
	pb "github.com/Livepeer-FrameWorks/monorepo/pkg/proto/ipc"
	"github.com/livepeer/go-livepeer/net"
)

func frameworksAccountingAmount(value *big.Rat) *pb.NetworkAmount {
	a := obs.AmountFromRat(value)
	if a == nil || a.Validate() != nil {
		return nil
	}
	return &pb.NetworkAmount{Numerator: a.Numerator, Denominator: a.Denominator, Currency: a.Currency, Unit: a.Unit}
}

// Untrusted result counts cannot wrap into an apparently valid throughput or charge.
func frameworksReturnedPixels(data *net.TranscodeData) (uint64, bool) {
	if data == nil {
		return 0, false
	}
	var total int64
	for _, segment := range data.Segments {
		if segment == nil || segment.Pixels < 0 || segment.Pixels > math.MaxInt64-total {
			return 0, false
		}
		total += segment.Pixels
	}
	return uint64(total), true
}
