package frankenphp

// #include "frankenphp.h"
import "C"
import (
	"sync"

	"github.com/dunglas/frankenphp/internal/phpheaders"
	"go.uber.org/zap"
)

// represents the main PHP thread
// the thread needs to keep running as long as all other threads are running
type phpMainThread struct {
	state           *threadState
	done            chan struct{}
	numThreads      int
	commonHeaders   map[string]*C.zend_string
	knownServerKeys map[string]*C.zend_string
}

var (
	phpThreads []*phpThread
	mainThread *phpMainThread
)

// reserve a fixed number of PHP threads on the Go side
func initPHPThreads(numThreads int) error {
	mainThread = &phpMainThread{
		state:      newThreadState(),
		done:       make(chan struct{}),
		numThreads: numThreads,
	}
	phpThreads = make([]*phpThread, numThreads)

	// initialize all threads as inactive
	// this needs to happen before starting the main thread
	// since some extensions access environment variables on startup
	for i := 0; i < numThreads; i++ {
		phpThreads[i] = newPHPThread(i)
		convertToInactiveThread(phpThreads[i])
	}

	if err := mainThread.start(); err != nil {
		return err
	}

	// start the underlying C threads
	ready := sync.WaitGroup{}
	ready.Add(numThreads)
	for _, thread := range phpThreads {
		go func() {
			if !C.frankenphp_new_php_thread(C.uintptr_t(thread.threadIndex)) {
				logger.Panic("unable to create thread", zap.Int("threadIndex", thread.threadIndex))
			}
			thread.state.waitFor(stateInactive)
			ready.Done()
		}()
	}
	ready.Wait()

	return nil
}

func drainPHPThreads() {
	doneWG := sync.WaitGroup{}
	doneWG.Add(len(phpThreads))
	for _, thread := range phpThreads {
		thread.handlerMu.Lock()
		_ = thread.state.requestSafeStateChange(stateShuttingDown)
		close(thread.drainChan)
	}
	close(mainThread.done)
	for _, thread := range phpThreads {
		go func(thread *phpThread) {
			thread.state.waitFor(stateDone)
			thread.handlerMu.Unlock()
			doneWG.Done()
		}(thread)
	}
	doneWG.Wait()
	mainThread.state.set(stateShuttingDown)
	mainThread.state.waitFor(stateDone)
	phpThreads = nil
}

func (mainThread *phpMainThread) start() error {
	if C.frankenphp_new_main_thread(C.int(mainThread.numThreads)) != 0 {
		return MainThreadCreationError
	}
	mainThread.state.waitFor(stateReady)

	// cache common request headers as zend_strings (HTTP_ACCEPT, HTTP_USER_AGENT, etc.)
	mainThread.commonHeaders = make(map[string]*C.zend_string, len(phpheaders.CommonRequestHeaders))
	for key, phpKey := range phpheaders.CommonRequestHeaders {
		mainThread.commonHeaders[key] = C.frankenphp_init_persistent_string(C.CString(phpKey), C.size_t(len(phpKey)))
	}

	// cache $_SERVER keys as zend_strings (SERVER_PROTOCOL, SERVER_SOFTWARE, etc.)
	mainThread.knownServerKeys = make(map[string]*C.zend_string, len(knownServerKeys))
	for _, phpKey := range knownServerKeys {
		mainThread.knownServerKeys[phpKey] = C.frankenphp_init_persistent_string(toUnsafeChar(phpKey), C.size_t(len(phpKey)))
	}

	return nil
}

func getInactivePHPThread() *phpThread {
	for _, thread := range phpThreads {
		if thread.state.is(stateInactive) {
			return thread
		}
	}
	panic("not enough threads reserved")
}

//export go_frankenphp_main_thread_is_ready
func go_frankenphp_main_thread_is_ready() {
	mainThread.state.set(stateReady)
	mainThread.state.waitFor(stateShuttingDown)
}

//export go_frankenphp_shutdown_main_thread
func go_frankenphp_shutdown_main_thread() {
	mainThread.state.set(stateDone)
}
