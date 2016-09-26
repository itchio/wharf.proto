package blockpool

import (
	"bufio"
	"bytes"
	"fmt"

	"github.com/go-errors/errors"
	"github.com/itchio/wharf/pwr"
	"github.com/itchio/wharf/splitfunc"
	"github.com/itchio/wharf/sync"
	"github.com/itchio/wharf/tlc"
)

// A SignatureInfo contains all the hashes for small-blocks of a given container
type SignatureInfo struct {
	container *tlc.Container
	hashes    []sync.BlockHash
}

// A ValidatingSink only stores blocks if they match the signature provided
// in Signature
type ValidatingSink struct {
	Sink      Sink
	Signature *SignatureInfo

	hashGroups map[BlockLocation][]sync.BlockHash
	blockBuf   []byte
	split      bufio.SplitFunc
	sctx       sync.Context
}

var _ Sink = (*ValidatingSink)(nil)

func (vs *ValidatingSink) Store(loc BlockLocation, data []byte) error {
	if vs.hashGroups == nil {
		err := vs.makeHashGroups()
		if err != nil {
			return errors.Wrap(err, 1)
		}

		vs.blockBuf = make([]byte, pwr.BlockSize)
		vs.split = splitfunc.New(pwr.BlockSize)
	}

	hashGroup := vs.hashGroups[loc]

	// see also sync.CreateSignature
	s := bufio.NewScanner(bytes.NewReader(data))
	s.Buffer(vs.blockBuf, 0)
	s.Split(vs.split)

	hashIndex := 0

	for ; s.Scan(); hashIndex++ {
		weakHash, strongHash := vs.sctx.HashBlock(s.Bytes())
		bh := hashGroup[hashIndex]

		if bh.WeakHash != weakHash {
			err := fmt.Errorf("at %+v, expected weak hash %x, got %x", loc, bh.WeakHash, weakHash)
			return errors.Wrap(err, 1)
		}

		if !bytes.Equal(bh.StrongHash, strongHash) {
			err := fmt.Errorf("at %+v, expected strong hash %x, got %x", loc, bh.StrongHash, strongHash)
			return errors.Wrap(err, 1)
		}
	}

	return vs.Sink.Store(loc, data)
}

func (vs *ValidatingSink) GetContainer() *tlc.Container {
	return vs.Sink.GetContainer()
}

func (vs *ValidatingSink) Clone() Sink {
	return &ValidatingSink{
		Sink:      vs.Sink,
		Signature: vs.Signature,
	}
}

func (vs *ValidatingSink) makeHashGroups() error {
	smallBlockSize := int64(pwr.BlockSize)

	pathToFileIndex := make(map[string]int64)
	for fileIndex, f := range vs.GetContainer().Files {
		pathToFileIndex[f.Path] = int64(fileIndex)
	}

	vs.hashGroups = make(map[BlockLocation][]sync.BlockHash)
	hashIndex := int64(0)

	for _, f := range vs.Signature.container.Files {
		fileIndex := pathToFileIndex[f.Path]

		if f.Size == 0 {
			// empty files have a 0-length shortblock for historical reasons.
			hashIndex++
			continue
		}

		numBigBlocks := ComputeNumBlocks(f.Size)
		for blockIndex := int64(0); blockIndex < numBigBlocks; blockIndex++ {
			loc := BlockLocation{
				FileIndex:  fileIndex,
				BlockIndex: blockIndex,
			}

			blockSize := ComputeBlockSize(f.Size, blockIndex)
			numSmallBlocks := (blockSize + smallBlockSize - 1) / smallBlockSize

			vs.hashGroups[loc] = vs.Signature.hashes[hashIndex : hashIndex+numSmallBlocks]
			hashIndex += numSmallBlocks
		}
	}

	return nil
}
