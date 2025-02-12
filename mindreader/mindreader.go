// Copyright 2019 dfuse Platform Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mindreader

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/streamingfast/bstream"
	"github.com/streamingfast/bstream/blockstream"
	"github.com/streamingfast/dstore"
	nodeManager "github.com/streamingfast/node-manager"
	"github.com/streamingfast/shutter"
	"go.uber.org/zap"
)

type ConsolerReader interface {
	Read() (obj interface{}, err error)
	Done() <-chan interface{}
}

type ConsolerReaderFactory func(lines chan string) (ConsolerReader, error)

// ConsoleReaderBlockTransformer is a function that accepts an `obj` of type
// `interface{}` as produced by a specialized ConsoleReader implementation and
// turns it into a `bstream.Block` that is able to flow in block streams.
type ConsoleReaderBlockTransformer func(obj interface{}) (*bstream.Block, error)

type MindReaderPlugin struct {
	*shutter.Shutter
	zlogger *zap.Logger

	startGate *BlockNumberGate // if set, discard blocks before this
	stopBlock uint64           // if set, call shutdownFunc(nil) when we hit this number

	waitUploadCompleteOnShutdown time.Duration // if non-zero, will try to upload files for this amount of time. Failed uploads will stay in workingDir

	lines         chan string
	consoleReader ConsolerReader // contains the 'reader' part of the pipe

	transformer     ConsoleReaderBlockTransformer // objects read from consoleReader are transformed into blocks
	channelCapacity int                           // transformed blocks are buffered in a channel

	archiver Archiver // transformed blocks are sent to Archiver

	consumeReadFlowDone chan interface{}
	continuityChecker   ContinuityChecker

	blockStreamServer    *blockstream.Server
	headBlockUpdateFunc  nodeManager.HeadBlockUpdater
	consoleReaderFactory ConsolerReaderFactory
}

// NewMindReaderPlugin initiates its own:
// * ConsoleReader (from given Factory)
// * ConsoleReaderBlockTransformer (from given Factory)
// * Archiver (from archive store params)
// * ContinuityChecker
// * Shutter
func NewMindReaderPlugin(
	archiveStoreURL string,
	mergeArchiveStoreURL string,
	batchMode bool,
	mergeThresholdBlockAge time.Duration,
	workingDirectory string,
	consoleReaderFactory ConsolerReaderFactory,
	consoleReaderTransformer ConsoleReaderBlockTransformer,
	tracker *bstream.Tracker,
	startBlockNum uint64,
	stopBlockNum uint64,
	channelCapacity int,
	headBlockUpdateFunc nodeManager.HeadBlockUpdater,
	shutdownFunc func(error),
	failOnNonContinuousBlocks bool,
	waitUploadCompleteOnShutdown time.Duration,
	oneblockSuffix string,
	blockStreamServer *blockstream.Server,
	zlogger *zap.Logger,
) (*MindReaderPlugin, error) {
	zlogger.Info("creating mindreader plugin",
		zap.String("archive_store_url", archiveStoreURL),
		zap.String("merge_archive_store_url", mergeArchiveStoreURL),
		zap.String("oneblock_suffix", oneblockSuffix),
		zap.Bool("batch_mode", batchMode),
		zap.Duration("merge_threshold_age", mergeThresholdBlockAge),
		zap.String("working_directory", workingDirectory),
		zap.Uint64("start_block_num", startBlockNum),
		zap.Uint64("stop_block_num", stopBlockNum),
		zap.Int("channel_capacity", channelCapacity),
		zap.Bool("with_head_block_update_func", headBlockUpdateFunc != nil),
		zap.Bool("with_shutdown_func", shutdownFunc != nil),
		zap.Bool("fail_on_non_continuous_blocks", failOnNonContinuousBlocks),
		zap.Duration("wait_upload_complete_on_shutdown", waitUploadCompleteOnShutdown),
	)

	// Create directory and its parent(s), it's a no-op if everything already exists
	err := os.MkdirAll(workingDirectory, os.ModePerm)
	if err != nil {
		return nil, fmt.Errorf("unable to create working directory %q: %w", workingDirectory, err)
	}

	oneblockArchiveStore, err := dstore.NewDBinStore(archiveStoreURL) // never overwrites
	if err != nil {
		return nil, fmt.Errorf("setting up archive store: %w", err)
	}
	oneBlockArchiver := NewOneBlockArchiver(oneblockArchiveStore, bstream.GetBlockWriterFactory, workingDirectory, oneblockSuffix, zlogger)

	mergeArchiveStore, err := dstore.NewDBinStore(mergeArchiveStoreURL)
	if err != nil {
		return nil, fmt.Errorf("setting up merge archive store: %w", err)
	}
	if batchMode {
		mergeArchiveStore.SetOverwrite(true)
	}

	mergeArchiver := NewMergeArchiver(mergeArchiveStore, bstream.GetBlockWriterFactory, workingDirectory, zlogger)

	archiverSelector := NewArchiverSelector(
		oneBlockArchiver,
		mergeArchiver,
		bstream.GetBlockReaderFactory,
		batchMode, tracker,
		mergeThresholdBlockAge,
		workingDirectory,
		zlogger,
	)
	if err := archiverSelector.Init(); err != nil {
		return nil, fmt.Errorf("failed to init archiver: %w", err)
	}

	mindReaderPlugin, err := newMindReaderPlugin(
		archiverSelector,
		consoleReaderFactory,
		consoleReaderTransformer,
		startBlockNum,
		stopBlockNum,
		channelCapacity,
		headBlockUpdateFunc,
		blockStreamServer,
		zlogger,
	)
	if err != nil {
		return nil, err
	}
	mindReaderPlugin.waitUploadCompleteOnShutdown = waitUploadCompleteOnShutdown

	return mindReaderPlugin, nil
}

func newMindReaderPlugin(
	archiver Archiver,
	consoleReaderFactory ConsolerReaderFactory,
	consoleReaderTransformer ConsoleReaderBlockTransformer,
	startBlock uint64,
	stopBlock uint64,
	channelCapacity int,
	headBlockUpdateFunc nodeManager.HeadBlockUpdater,
	blockStreamServer *blockstream.Server,
	zlogger *zap.Logger,
) (*MindReaderPlugin, error) {
	zlogger.Info("creating new mindreader plugin")
	return &MindReaderPlugin{
		Shutter:              shutter.New(),
		consoleReaderFactory: consoleReaderFactory,
		transformer:          consoleReaderTransformer,
		archiver:             archiver,
		startGate:            NewBlockNumberGate(startBlock),
		stopBlock:            stopBlock,
		channelCapacity:      channelCapacity,
		headBlockUpdateFunc:  headBlockUpdateFunc,
		zlogger:              zlogger,
		blockStreamServer:    blockStreamServer,
	}, nil
}

func (p *MindReaderPlugin) Name() string {
	return "MindReaderPlugin"
}

func (p *MindReaderPlugin) Launch() {
	p.zlogger.Info("starting mindreader")

	p.consumeReadFlowDone = make(chan interface{})
	blocks := make(chan *bstream.Block, p.channelCapacity)

	lines := make(chan string, 10000) //need a config here?
	p.lines = lines

	consoleReader, err := p.consoleReaderFactory(lines)
	if err != nil {
		p.Shutdown(err)
	}
	p.consoleReader = consoleReader

	go p.consumeReadFlow(blocks)
	go p.archiver.Start()

	go func() {
		for {
			// Always read messages otherwise you'll stall the shutdown lifecycle of the managed process, leading to corrupted database if exit uncleanly afterward
			err := p.readOneMessage(blocks)
			if err != nil {
				if err == io.EOF {
					p.zlogger.Info("reached end of console reader stream, nothing more to do")
					close(blocks)
					return
				}
				p.zlogger.Error("reading from console logs", zap.Error(err))
				p.Shutdown(err)
				continue
			}
		}
	}()
}

func (p MindReaderPlugin) Stop() {
	p.zlogger.Info("mindreader is stopping")
	if p.lines == nil {
		// If the `lines` channel was not created yet, it means everything was shut down very rapidly
		// and means MindreaderPlugin has not launched yet. Since it has not launched yet, there is
		// no point in waiting for the read flow to complete since the read flow never started. So
		// we exit right now.
		return
	}

	close(p.lines)
	p.waitForReadFlowToComplete()
}

func (p *MindReaderPlugin) waitForReadFlowToComplete() {
	p.zlogger.Info("waiting until consume read flow (i.e. blocks) is actually done processing blocks...")
	<-p.consumeReadFlowDone
	p.zlogger.Info("consume read flow terminate")
}

// consumeReadFlow is the one function blocking termination until consumption/writeBlock/upload is done
func (p *MindReaderPlugin) consumeReadFlow(blocks <-chan *bstream.Block) {
	p.zlogger.Info("starting consume flow")
	defer close(p.consumeReadFlowDone)

	for {
		p.zlogger.Debug("waiting to consume next block.")
		block, ok := <-blocks
		if !ok {
			p.zlogger.Info("all blocks in channel were drained, exiting read flow")
			p.archiver.Shutdown(nil)
			select {
			case <-time.After(p.waitUploadCompleteOnShutdown):
				p.zlogger.Info("upload may not be complete: timeout waiting for UploadComplete on shutdown", zap.Duration("wait_upload_complete_on_shutdown", p.waitUploadCompleteOnShutdown))
			case <-p.archiver.Terminated():
				p.zlogger.Info("archiver Terminate done")
			}

			return
		}

		p.zlogger.Debug("got one block", zap.Uint64("block_num", block.Number))

		err := p.archiver.StoreBlock(block)
		if err != nil {
			p.zlogger.Error("failed storing block in archiver, shutting down. You will need to reprocess over this range to get this block.", zap.Error(err), zap.Stringer("block", block))
			if !p.IsTerminating() {
				go p.Shutdown(fmt.Errorf("archiver store block failed: %w", err))
				continue
			}
		}
		if p.blockStreamServer != nil {
			err = p.blockStreamServer.PushBlock(block)
			if err != nil {
				p.zlogger.Error("failed passing block to blockStreamServer (this should not happen, shutting down)", zap.Error(err))
				if !p.IsTerminating() {
					go p.Shutdown(fmt.Errorf("blockstreamserver failed: %w", err))
				}

				continue
			}
		}

		if p.continuityChecker != nil {
			err = p.continuityChecker.Write(block.Num())
			if err != nil {
				p.zlogger.Error("failed continuity check", zap.Error(err))

				if !p.IsTerminating() {
					go p.Shutdown(fmt.Errorf("continuity check failed: %w", err))
				}
				continue
			}
		}
	}
}

func (p *MindReaderPlugin) readOneMessage(blocks chan<- *bstream.Block) error {
	obj, err := p.consoleReader.Read()
	if err != nil {
		return err
	}

	block, err := p.transformer(obj)
	if err != nil {
		return fmt.Errorf("unable to transform console read obj to bstream.Block: %w", err)
	}

	if !p.startGate.pass(block) {
		return nil
	}

	if p.headBlockUpdateFunc != nil {
		p.headBlockUpdateFunc(block.Num(), block.ID(), block.Time())
	}

	blocks <- block

	if p.stopBlock != 0 && block.Num() >= p.stopBlock && !p.IsTerminating() {
		p.zlogger.Info("shutting down because requested end block reached", zap.Uint64("block_num", block.Num()))
		go p.Shutdown(nil)
	}

	return nil
}

// LogLine receives log line and write it to "pipe" of the local console reader
func (p *MindReaderPlugin) LogLine(in string) {
	if p.IsTerminating() {
		return
	}
	p.lines <- in
}

func (p *MindReaderPlugin) HasContinuityChecker() bool {
	return p.continuityChecker != nil
}

func (p *MindReaderPlugin) ResetContinuityChecker() {
	if p.continuityChecker != nil {
		p.continuityChecker.Reset()
	}
}
