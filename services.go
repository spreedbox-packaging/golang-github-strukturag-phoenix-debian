package phoenix

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// Service represents a resource whose lifecycle should be managed by a Runtime.
//
// Typically this would be an exclusive resource such as a socket, database file,
// or shared memory segment.
type Service interface {
	// Start runs the main loop of the Service. It is expected to block until
	// Stop is called or the execution of the service is complete.
	//
	// Undefined behavior will result if errors are returned during shutdown,
	// such errors shall be returned by Stop.
	Start() error

	// Stop shall terminate execution of Start and may return any errors
	// reported by cleanup of resources used by the Service.
	Stop() error
}

// Reloadable should be implemented by services which wish to respond to
// configuration reload requests.
type Reloadable interface {
	// Reload will be called when the server's configuration has been reloaded.
	//
	// If any reloadable service returns an error, the server will be stopped.
	Reload() error
}

// StartHandler may be implemented by services which wish to be notified prior
// to being started.
type StartHandler interface {
	// OnStart receives the current container, and may return an error to cancel
	// startup.
	OnStart(Container) error
}

// StopHandler may be implemented by services which wish to be notified after
// they stop.
type StopHandler interface {
	OnStop(Container)
}

type serviceManager struct {
	*container
	services []Service
}

func newServiceManager(container *container) *serviceManager {
	return &serviceManager{
		container,
		make([]Service, 0, 1),
	}
}

func (manager *serviceManager) AddService(service Service) {
	manager.services = append(manager.services, service)
}

func (manager *serviceManager) Start() error {
	if len(manager.services) <= 0 {
		return errors.New("no services were registered")
	}

	running := &sync.WaitGroup{}
	fail := make(chan error, len(manager.services))

	for _, service := range manager.services {
		running.Add(1)
		go func(srv Service) {
			defer running.Done()

			if handler, ok := srv.(StartHandler); ok {
				if err := handler.OnStart(manager); err != nil {
					fail <- err
					return
				}
			}

			if err := srv.Start(); err != nil {
				manager.Printf("Error while listening %s\n", err)
				fail <- err
			} else if handler, ok := srv.(StopHandler); ok {
				handler.OnStop(manager)
			}
		}(service)
	}

	done := make(chan bool)
	go func() {
		running.Wait()
		close(done)
	}()

	faults := &multiError{}
	select {
	case <-done:
	case err := <-fail:
		faults.AddError(err)
		// NOTE(lcooper): We'll bail eventually, collect all errors first.
	Loop:
		for {
			select {
			case err := <-fail:
				faults.AddError(err)
			case <-time.After(500 * time.Millisecond):
				break Loop
			}
		}
	}

	return faults.AsError()
}

func (manager *serviceManager) Reload() error {
	if err := manager.config.load(); err != nil {
		return err
	}

	failedToReload := &multiError{}
	for _, service := range manager.services {
		if reloadable, ok := service.(Reloadable); ok {
			failedToReload.AddError(reloadable.Reload())
		}
	}

	return failedToReload.AsError()
}

func (manager *serviceManager) Stop() error {
	faults := &multiError{}
	stopping := sync.WaitGroup{}
	for i := len(manager.services) -1; i >=0; i-- {
		service := manager.services[i]
		fault := make(chan error, 1)
		stopping.Add(1)
		go func() {
			fault <- service.Stop()
		}()

		go func() {
			defer stopping.Done()
			var err error
			select {
			case err = <- fault:
			case <- time.After(5 * time.Second):
				err = errors.New("timed out waiting for service to stop")
			}
			faults.AddError(err)
		}()
	}

	stopping.Wait()
	return faults.AsError()
}

type multiError struct {
	sync.Mutex
	errors []error
}

func (stop *multiError) AddError(err error) {
	if err != nil {
		stop.Lock()
		defer stop.Unlock()
		stop.errors = append(stop.errors, err)
	}
}

func (stop *multiError) Error() string {
	stop.Lock()
	defer stop.Unlock()

	msgs := make([]string, 0, len(stop.errors))
	for _, err := range stop.errors {
		msgs = append(msgs, err.Error())
	}
	return strings.Join(msgs, "\n")
}

func (stop *multiError) AsError() error {
	stop.Lock()
	defer stop.Unlock()

	if len(stop.errors) == 0 {
		return nil
	}
	return stop
}
