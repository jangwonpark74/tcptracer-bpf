// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2018 Datadog, Inc.

package file

import (
	"time"

	"github.com/DataDog/datadog-agent/pkg/util/log"

	"github.com/DataDog/datadog-agent/pkg/logs/auditor"
	"github.com/DataDog/datadog-agent/pkg/logs/config"
	"github.com/DataDog/datadog-agent/pkg/logs/message"
	"github.com/DataDog/datadog-agent/pkg/logs/pipeline"
	"github.com/DataDog/datadog-agent/pkg/logs/restart"
)

// scanPeriod represents the period of time between two scans.
const scanPeriod = 10 * time.Second

// Scanner checks all files provided by fileProvider and create new tailers
// or update the old ones if needed
type Scanner struct {
	pipelineProvider    pipeline.Provider
	tailingLimit        int
	fileProvider        *Provider
	tailers             map[string]*Tailer
	registry            auditor.Registry
	tailerSleepDuration time.Duration
	stop                chan struct{}
}

// NewScanner returns a new scanner.
func NewScanner(sources *config.LogSources, tailingLimit int, pipelineProvider pipeline.Provider, registry auditor.Registry, tailerSleepDuration time.Duration) *Scanner {
	return &Scanner{
		pipelineProvider:    pipelineProvider,
		tailingLimit:        tailingLimit,
		fileProvider:        NewProvider(sources, tailingLimit),
		tailers:             make(map[string]*Tailer),
		registry:            registry,
		tailerSleepDuration: tailerSleepDuration,
		stop:                make(chan struct{}),
	}
}

// Start starts the scanner.
func (s *Scanner) Start() {
	s.setup()
	go s.run()
}

// Stop stops the scanner.
func (s *Scanner) Stop() {
	s.stop <- struct{}{}
	s.cleanup()
}

// setup sets all tailers
func (s *Scanner) setup() {
	files := s.fileProvider.FilesToTail()
	for _, file := range files {
		if len(s.tailers) == s.tailingLimit {
			return
		}
		if _, ok := s.tailers[file.Path]; ok {
			log.Warn("Can't tail file twice: ", file.Path)
		} else {
			// resume tailing from last committed offset if exists or start tailing from the end of file otherwise
			// to prevent from reading a file over and over again at agent restart
			s.startNewTailer(file, false)
		}
	}
}

// run checks periodically if there are new files to tail and the state of its tailers until stop
func (s *Scanner) run() {
	scanTicker := time.NewTicker(scanPeriod)
	defer scanTicker.Stop()
	for {
		select {
		case <-scanTicker.C:
			// check if there are new files to tail, tailers to stop and tailer to restart because of file rotation
			s.scan()
		case <-s.stop:
			// no more file should be tailed
			return
		}
	}
}

// cleanup all tailers
func (s *Scanner) cleanup() {
	stopper := restart.NewParallelStopper()
	for _, tailer := range s.tailers {
		stopper.Add(tailer)
		delete(s.tailers, tailer.path)
	}
	stopper.Stop()
}

// scan checks all the files we're expected to tail,
// compares them to the currently tailed files,
// and triggeres the required updates.
// For instance, when a file is logrotated,
// its tailer will keep tailing the rotated file.
// The Scanner needs to stop that previous tailer,
// and start a new one for the new file.
func (s *Scanner) scan() {
	files := s.fileProvider.FilesToTail()
	filesTailed := make(map[string]bool)
	tailersLen := len(s.tailers)

	for _, file := range files {
		tailer, isTailed := s.tailers[file.Path]
		if isTailed && tailer.shouldStop {
			// skip this tailer as it must be stopped
			continue
		}
		if !isTailed && tailersLen >= s.tailingLimit {
			// can't create new tailer because tailingLimit is reached
			continue
		}

		if !isTailed && tailersLen < s.tailingLimit {
			// create a new tailer tailing from the beginning of the file if no offset has been recorded
			succeeded := s.startNewTailer(file, true)
			if !succeeded {
				// the setup failed, let's try to tail this file in the next scan
				continue
			}
			tailersLen++
			filesTailed[file.Path] = true
			continue
		}

		didRotate, err := DidRotate(tailer.file, tailer.GetReadOffset())
		if err != nil {
			continue
		}
		if didRotate {
			// restart tailer because of file-rotation on file
			succeeded := s.restartTailerAfterFileRotation(tailer, file)
			if !succeeded {
				// the setup failed, let's try to tail this file in the next scan
				continue
			}
		}

		filesTailed[file.Path] = true
	}

	for path, tailer := range s.tailers {
		// stop all tailers which have not been selected
		_, shouldTail := filesTailed[path]
		if !shouldTail {
			s.stopTailer(tailer)
		}
	}
}

// startNewTailer creates a new tailer, making it tail from the last committed offset, the beginning or the end of the file,
// returns true if the operation succeeded, false otherwise
func (s *Scanner) startNewTailer(file *File, tailFromBeginning bool) bool {
	tailer := s.createTailer(file, s.pipelineProvider.NextPipelineChan())

	offset, whence, err := Position(s.registry, tailer.Identifier(), tailFromBeginning)
	if err != nil {
		log.Warnf("Could not recover offset for file with path %v: %v", file.Path, err)
	}

	err = tailer.Start(offset, whence)
	if err != nil {
		log.Warn(err)
		return false
	}

	s.tailers[file.Path] = tailer
	return true
}

// stopTailer stops the tailer
func (s *Scanner) stopTailer(tailer *Tailer) {
	go tailer.Stop()
	delete(s.tailers, tailer.path)
}

// restartTailer safely stops tailer and starts a new one
// returns true if the new tailer is up and running, false if an error occurred
func (s *Scanner) restartTailerAfterFileRotation(tailer *Tailer, file *File) bool {
	log.Info("Log rotation happened to ", tailer.path)
	tailer.StopAfterFileRotation()
	tailer = s.createTailer(file, tailer.outputChan)
	// force reading file from beginning since it has been log-rotated
	err := tailer.StartFromBeginning()
	if err != nil {
		log.Warn(err)
		return false
	}
	s.tailers[file.Path] = tailer
	return true
}

// createTailer returns a new tailer.
func (s *Scanner) createTailer(file *File, outputChan chan message.Message) *Tailer {
	return NewTailer(outputChan, file.Source, file.Path, s.tailerSleepDuration)
}