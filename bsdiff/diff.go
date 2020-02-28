package bsdiff

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/itchio/headway/state"
	"github.com/itchio/headway/united"
	"github.com/pkg/errors"
)

// A Match is a pair of two regions from the old and new file that have been
// selected by the bsdiff algorithm for subtraction.
type Match struct {
	addOldStart int
	addNewStart int
	addLength   int
	copyEnd     int
	eoc         bool
}

type blockWorkerState struct {
	consumed chan bool
	work     chan int
	matches  chan Match
}

func (m Match) copyStart() int {
	return m.addNewStart + m.addLength
}

// MaxFileSize is the largest size bsdiff will diff (for both old and new file): 2GB - 1 bytes
// a different codepath could be used for larger files, at the cost of unreasonable memory usage
// (even in 2016). If a big corporate user is willing to sponsor that part of the code, get in touch!
// Fair warning though: it won't be covered, our CI workers don't have that much RAM :)
const MaxFileSize = int64(math.MaxInt32 - 1)

// MaxMessageSize is the maximum amount of bytes that will be stored
// in a protobuf message generated by bsdiff. This enable friendlier streaming apply
// at a small storage cost
// TODO: actually use
const MaxMessageSize int64 = 16 * 1024 * 1024

// DiffContext holds settings for the diff process, along with some
// internal storage: re-using a diff context is good to avoid GC thrashing
// (but never do it concurrently!)
type DiffContext struct {
	// SuffixSortConcurrency specifies the number of workers to use for suffix sorting.
	// Exceeding the number of cores will only slow it down. A 0 value (default) uses
	// sequential suffix sorting, which uses less RAM and has less overhead (might be faster
	// in some scenarios). A negative value means (number of cores - value).
	SuffixSortConcurrency int

	// number of partitions into which to separate the input data, sort concurrently
	// and scan in concurrently
	Partitions int

	// MeasureMem enables printing memory usage statistics at various points in the
	// diffing process.
	MeasureMem bool

	// MeasureParallelOverhead prints some stats on the overhead of parallel suffix sorting
	MeasureParallelOverhead bool

	Stats *DiffStats

	db bytes.Buffer

	obuf bytes.Buffer
	nbuf bytes.Buffer

	I []int
}

type DiffStats struct {
	TimeSpentSorting  time.Duration
	TimeSpentScanning time.Duration
	BiggestAdd        int64
}

// WriteMessageFunc should write a given protobuf message and relay any errors
// No reference to the given message can be kept, as its content may be modified
// after WriteMessageFunc returns. See the `wire` package for an example implementation.
type WriteMessageFunc func(msg proto.Message) (err error)

func (ctx *DiffContext) writeMessages(obuf []byte, nbuf []byte, matches chan Match, writeMessage WriteMessageFunc) error {
	var err error

	bsdc := &Control{}

	var prevMatch Match
	first := true

	for match := range matches {
		if first {
			first = false
		} else {
			bsdc.Seek = int64(match.addOldStart - (prevMatch.addOldStart + prevMatch.addLength))

			err := writeMessage(bsdc)
			if err != nil {
				return err
			}
		}

		ctx.db.Reset()
		ctx.db.Grow(match.addLength)

		for i := 0; i < match.addLength; i++ {
			ctx.db.WriteByte(nbuf[match.addNewStart+i] - obuf[match.addOldStart+i])
		}

		bsdc.Add = ctx.db.Bytes()
		bsdc.Copy = nbuf[match.copyStart():match.copyEnd]

		if ctx.Stats != nil && ctx.Stats.BiggestAdd < int64(len(bsdc.Add)) {
			ctx.Stats.BiggestAdd = int64(len(bsdc.Add))
		}

		prevMatch = match
	}

	bsdc.Seek = 0
	err = writeMessage(bsdc)
	if err != nil {
		return err
	}

	bsdc.Reset()
	bsdc.Eof = true
	err = writeMessage(bsdc)
	if err != nil {
		return err
	}

	return nil
}

// Do computes the difference between old and new, according to the bsdiff
// algorithm, and writes the result to patch.
func (ctx *DiffContext) Do(old, new io.Reader, writeMessage WriteMessageFunc, consumer *state.Consumer) error {
	var memstats *runtime.MemStats
	var err error

	if ctx.MeasureMem {
		memstats = &runtime.MemStats{}
		runtime.ReadMemStats(memstats)
		fmt.Fprintf(os.Stderr, "\nAllocated bytes at start of bsdiff: %s (%s total)", united.FormatBytes(int64(memstats.Alloc)), united.FormatBytes(int64(memstats.TotalAlloc)))
	}

	ctx.obuf.Reset()
	_, err = io.Copy(&ctx.obuf, old)
	if err != nil {
		return err
	}

	obuf := ctx.obuf.Bytes()
	obuflen := ctx.obuf.Len()

	ctx.nbuf.Reset()
	_, err = io.Copy(&ctx.nbuf, new)
	if err != nil {
		return err
	}

	nbuf := ctx.nbuf.Bytes()
	nbuflen := ctx.nbuf.Len()
	if nbuflen == 0 {
		// empty "new" file, only write EOF message
		bsdc := &Control{}
		bsdc.Eof = true
		err := writeMessage(bsdc)
		if err != nil {
			return err
		}
		return nil
	}

	matches := make(chan Match, 256)

	if ctx.MeasureMem {
		runtime.ReadMemStats(memstats)
		fmt.Fprintf(os.Stderr, "\nAllocated bytes after ReadAll: %s (%s total)", united.FormatBytes(int64(memstats.Alloc)), united.FormatBytes(int64(memstats.TotalAlloc)))
	}

	partitions := ctx.Partitions
	if partitions == 0 || partitions >= len(obuf)-1 {
		partitions = 1
	}

	consumer.ProgressLabel(fmt.Sprintf("Sorting %s...", united.FormatBytes(int64(obuflen))))
	consumer.Progress(0.0)

	startTime := time.Now()

	if ctx.I == nil || len(ctx.I) < len(obuf) {
		ctx.I = make([]int, len(obuf))
	}

	psa := NewPSA(partitions, obuf, ctx.I)

	if ctx.Stats != nil {
		ctx.Stats.TimeSpentSorting += time.Since(startTime)
	}

	if ctx.MeasureMem {
		runtime.ReadMemStats(memstats)
		fmt.Fprintf(os.Stderr, "\nAllocated bytes after qsufsort: %s (%s total)", united.FormatBytes(int64(memstats.Alloc)), united.FormatBytes(int64(memstats.TotalAlloc)))
	}

	consumer.ProgressLabel(fmt.Sprintf("Preparing to scan %s...", united.FormatBytes(int64(nbuflen))))
	consumer.Progress(0.0)

	startTime = time.Now()

	analyzeBlock := func(nbuflen int, nbuf []byte, offset int, blockMatches chan Match) {
		var lenf int

		// Compute the differences, writing ctrl as we go
		var scan, pos, length int
		var lastscan, lastpos, lastoffset int

		for scan < nbuflen {
			var oldscore int
			scan += length

			for scsc := scan; scan < nbuflen; scan++ {
				pos, length = psa.search(nbuf[scan:])

				for ; scsc < scan+length; scsc++ {
					if scsc+lastoffset < obuflen &&
						obuf[scsc+lastoffset] == nbuf[scsc] {
						oldscore++
					}
				}

				if (length == oldscore && length != 0) || length > oldscore+8 {
					break
				}

				if scan+lastoffset < obuflen && obuf[scan+lastoffset] == nbuf[scan] {
					oldscore--
				}
			}

			if length != oldscore || scan == nbuflen {
				var s, Sf int
				lenf = 0
				for i := int(0); lastscan+i < scan && lastpos+i < obuflen; {
					if obuf[lastpos+i] == nbuf[lastscan+i] {
						s++
					}
					i++
					if s*2-i > Sf*2-lenf {
						Sf = s
						lenf = i
					}
				}

				lenb := 0
				if scan < nbuflen {
					var s, Sb int
					for i := int(1); (scan >= lastscan+i) && (pos >= i); i++ {
						if obuf[pos-i] == nbuf[scan-i] {
							s++
						}
						if s*2-i > Sb*2-lenb {
							Sb = s
							lenb = i
						}
					}
				}

				if lastscan+lenf > scan-lenb {
					overlap := (lastscan + lenf) - (scan - lenb)
					s := int(0)
					Ss := int(0)
					lens := int(0)
					for i := int(0); i < overlap; i++ {
						if nbuf[lastscan+lenf-overlap+i] == obuf[lastpos+lenf-overlap+i] {
							s++
						}
						if nbuf[scan-lenb+i] == obuf[pos-lenb+i] {
							s--
						}
						if s > Ss {
							Ss = s
							lens = i + 1
						}
					}

					lenf += lens - overlap
					lenb -= lens
				}

				m := Match{
					addOldStart: lastpos,
					addNewStart: lastscan + offset,
					addLength:   lenf,
					copyEnd:     scan - lenb + offset,
				}

				// if not a no-op, send
				blockMatches <- m

				lastscan = scan - lenb
				lastpos = pos - lenb
				lastoffset = pos - scan
			}
		}

		blockMatches <- Match{eoc: true}
	}

	blockSize := 128 * 1024
	numBlocks := (nbuflen + blockSize - 1) / blockSize

	if numBlocks < partitions {
		blockSize = nbuflen / partitions
		numBlocks = (nbuflen + blockSize - 1) / blockSize
	}

	// TODO: figure out exactly how much overkill that is
	numWorkers := partitions * 12
	if numWorkers > numBlocks {
		numWorkers = numBlocks
	}

	blockWorkersState := make([]blockWorkerState, numWorkers)

	// initialize all channels
	for i := 0; i < numWorkers; i++ {
		blockWorkersState[i].work = make(chan int, 1)
		blockWorkersState[i].matches = make(chan Match, 256)
		blockWorkersState[i].consumed = make(chan bool, 1)
		blockWorkersState[i].consumed <- true
	}

	// spin up workers
	for i := 0; i < numWorkers; i++ {
		go func(workerState blockWorkerState, workerIndex int) {
			for blockIndex := range workerState.work {
				boundary := blockSize * blockIndex
				realBlockSize := blockSize
				if blockIndex == numBlocks-1 {
					realBlockSize = nbuflen - boundary
				}

				analyzeBlock(realBlockSize, nbuf[boundary:boundary+realBlockSize], boundary, workerState.matches)
			}
		}(blockWorkersState[i], i)
	}

	// dispatch work to workers
	go func() {
		workerIndex := 0

		for i := 0; i < numBlocks; i++ {
			<-blockWorkersState[workerIndex].consumed
			blockWorkersState[workerIndex].work <- i

			workerIndex = (workerIndex + 1) % numWorkers
		}

		for workerIndex := 0; workerIndex < numWorkers; workerIndex++ {
			close(blockWorkersState[workerIndex].work)
		}
		// fmt.Fprintf(os.Stderr, "Sent all blockworks\n")
	}()

	if ctx.MeasureMem {
		runtime.ReadMemStats(memstats)
		fmt.Fprintf(os.Stderr, "\nAllocated bytes after scan-prepare: %s (%s total)", united.FormatBytes(int64(memstats.Alloc)), united.FormatBytes(int64(memstats.TotalAlloc)))
	}

	consumer.ProgressLabel(fmt.Sprintf("Scanning %s (%d blocks of %s)...", united.FormatBytes(int64(nbuflen)), numBlocks, united.FormatBytes(int64(blockSize))))

	// collect workers' results, forward them to consumer
	go func() {
		workerIndex := 0
		for blockIndex := 0; blockIndex < numBlocks; blockIndex++ {
			consumer.Progress(float64(blockIndex) / float64(numBlocks))
			state := blockWorkersState[workerIndex]

			for match := range state.matches {
				if match.eoc {
					break
				}

				matches <- match
			}

			state.consumed <- true
			workerIndex = (workerIndex + 1) % numWorkers
		}

		close(matches)
	}()

	err = ctx.writeMessages(obuf, nbuf, matches, writeMessage)
	if err != nil {
		return errors.WithStack(err)
	}

	if ctx.Stats != nil {
		ctx.Stats.TimeSpentScanning += time.Since(startTime)
	}

	if ctx.MeasureMem {
		runtime.ReadMemStats(memstats)
		consumer.Debugf("\nAllocated bytes after scan: %s (%s total)", united.FormatBytes(int64(memstats.Alloc)), united.FormatBytes(int64(memstats.TotalAlloc)))
		fmt.Fprintf(os.Stderr, "\nAllocated bytes after scan: %s (%s total)", united.FormatBytes(int64(memstats.Alloc)), united.FormatBytes(int64(memstats.TotalAlloc)))
	}

	return nil

}
