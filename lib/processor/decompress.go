// Copyright (c) 2018 Ashley Jeffs
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package processor

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"

	"github.com/Jeffail/benthos/lib/types"
	"github.com/Jeffail/benthos/lib/util/service/log"
	"github.com/Jeffail/benthos/lib/util/service/metrics"
)

//------------------------------------------------------------------------------

func init() {
	Constructors["decompress"] = TypeSpec{
		constructor: NewDecompress,
		description: `
Decompresses the parts of a message according to the selected algorithm.
Supported decompression types are: gzip (I'll add more later). If the list of
target parts is empty the decompression will be applied to all message parts.

Part indexes can be negative, and if so the part will be selected from the end
counting backwards starting from -1. E.g. if index = -1 then the selected part
will be the last part of the message, if index = -2 then the part before the
last element with be selected, and so on.

Parts that fail to decompress (invalid format) will be removed from the message.
If the message results in zero parts it is skipped entirely.`,
	}
}

//------------------------------------------------------------------------------

// DecompressConfig contains any configuration for the Decompress processor.
type DecompressConfig struct {
	Algorithm string `json:"algorithm" yaml:"algorithm"`
	Parts     []int  `json:"parts" yaml:"parts"`
}

// NewDecompressConfig returns a DecompressConfig with default values.
func NewDecompressConfig() DecompressConfig {
	return DecompressConfig{
		Algorithm: "gzip",
		Parts:     []int{},
	}
}

//------------------------------------------------------------------------------

type decompressFunc func(bytes []byte) ([]byte, error)

func gzipDecompress(b []byte) ([]byte, error) {
	buf := bytes.NewBuffer(b)
	zr, err := gzip.NewReader(buf)
	if err != nil {
		return nil, err
	}

	outBuf := bytes.Buffer{}
	if _, err = outBuf.ReadFrom(zr); err != nil && err != io.EOF {
		return nil, err
	}
	zr.Close()
	return outBuf.Bytes(), nil
}

func strToDecompressor(str string) (decompressFunc, error) {
	switch str {
	case "gzip":
		return gzipDecompress, nil
	}
	return nil, fmt.Errorf("decompression type not recognised: %v", str)
}

//------------------------------------------------------------------------------

// Decompress is a processor that can selectively decompress parts of a message
// as a chosen compression algorithm.
type Decompress struct {
	conf   DecompressConfig
	decomp decompressFunc

	log   log.Modular
	stats metrics.Type
}

// NewDecompress returns a Decompress processor.
func NewDecompress(conf Config, log log.Modular, stats metrics.Type) (Type, error) {
	dcor, err := strToDecompressor(conf.Decompress.Algorithm)
	if err != nil {
		return nil, err
	}
	return &Decompress{
		conf:   conf.Decompress,
		decomp: dcor,
		log:    log.NewModule(".processor.decompress"),
		stats:  stats,
	}, nil
}

//------------------------------------------------------------------------------

// ProcessMessage takes a message, attempts to decompress parts of the message,
// and returns the result.
func (d *Decompress) ProcessMessage(msg types.Message) ([]types.Message, types.Response) {
	d.stats.Incr("processor.decompress.count", 1)

	newMsg := types.Message{}
	lParts := len(msg.Parts)

	noParts := len(d.conf.Parts) == 0
	for i, part := range msg.Parts {
		isTarget := noParts
		if !isTarget {
			nI := i - lParts
			for _, t := range d.conf.Parts {
				if t == nI || t == i {
					isTarget = true
					break
				}
			}
		}
		if !isTarget {
			newMsg.Parts = append(newMsg.Parts, part)
			continue
		}
		newPart, err := d.decomp(part)
		if err == nil {
			d.stats.Incr("processor.decompress.success", 1)
			newMsg.Parts = append(newMsg.Parts, newPart)
		} else {
			d.stats.Incr("processor.decompress.error", 1)
		}
	}

	if len(newMsg.Parts) == 0 {
		d.stats.Incr("processor.decompress.skipped", 1)
		return nil, types.NewSimpleResponse(nil)
	}

	d.stats.Incr("processor.decompress.sent", 1)
	msgs := [1]types.Message{newMsg}
	return msgs[:], nil
}

//------------------------------------------------------------------------------
