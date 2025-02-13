package common

import (
	"math/big"

	ethCommon "github.com/ethereum/go-ethereum/common"
)

const (
	// BytesLength to represent the depth of merkle tree
	NLevelsAsBytes = 3
)

// EthAddrToBigInt returns a *big.Int from a given ethereum common.Address.
func EthAddrToBigInt(a ethCommon.Address) *big.Int {
	return new(big.Int).SetBytes(a.Bytes())
}

// SwapEndianness swaps the order of the bytes in the slice.
func SwapEndianness(b []byte) []byte {
	o := make([]byte, len(b))
	for i := range b {
		o[len(b)-1-i] = b[i]
	}
	return o
}

// CopyBigInt returns a copy of the big int
func CopyBigInt(a *big.Int) *big.Int {
	return new(big.Int).SetBytes(a.Bytes())
}
