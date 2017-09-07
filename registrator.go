package main

import (
	"encoding/json"
	"flag"
	"fmt"
	dockerApi "github.com/docker/engine-api/client"
	"github.com/docker/engine-api/types"
	"github.com/docker/engine-api/types/events"
	"github.com/docker/go-connections/nat"
	consulApi "github.com/hashicorp/consul/api"
	"golang.org/x/net/context"
	"io"
	"log"
	"strconv"
	"sync"
)

const (
	CreateEvent                = "create"
	StartEvent                 = "start"
	StopEvent                  = "stop"
	PauseEvent                 = "pause"
	UnpauseEvent               = "unpause"
	KillEvent                  = "kill"
	DestroyEvent               = "destroy"
	DieEvent                   = "die"
	OutOfMemoryEvent           = "oom"
	ContainerPublicNameLabel   = "service.name"
	ContainerPublicPortLabel   = "service.port"
	ConsulServiceCheckInterval = "15s"
	ConsulServiceCheckTimeout  = "30s"
	ConsulServiceTTL           = "45s"
	DockerApiVersion           = "v1.22"
	DockerApiHost              = "unix:///var/run/docker.sock"
	ApplicationName            = "docker-library-consul-registrator"
)

func NewHandler(fun func(events.Message) string) *EventHandler {
	return &EventHandler{
		keyFunc:  fun,
		handlers: make(map[string]func(events.Message)),
	}
}

func ByType(e events.Message) string {
	return e.Type
}

func ByAction(e events.Message) string {
	return e.Action
}

type EventHandler struct {
	keyFunc  func(events.Message) string
	handlers map[string]func(events.Message)
	mu       sync.Mutex
}

func (w *EventHandler) Handle(key string, h func(events.Message)) {
	w.mu.Lock()
	w.handlers[key] = h
	w.mu.Unlock()
}

func (w *EventHandler) Watch(c <-chan events.Message) {
	for e := range c {
		w.mu.Lock()
		h, exists := w.handlers[w.keyFunc(e)]
		w.mu.Unlock()
		if !exists {
			continue
		}
		go h(e)
	}
}

type eventProcessor func(event events.Message, err error) error

func decodeEvents(input io.Reader, ep eventProcessor) error {
	dec := json.NewDecoder(input)
	for {
		var event events.Message
		err := dec.Decode(&event)
		if err != nil && err == io.EOF {
			break
		}

		if procErr := ep(event, err); procErr != nil {
			return procErr
		}
	}
	return nil
}

func monitorEvents(
	ctx context.Context,
	cli dockerApi.APIClient,
	options types.EventsOptions,
	started chan struct{},
	eventChan chan events.Message,
	errChan chan error) {
	body, err := cli.Events(ctx, options)

	close(started)
	if err != nil {
		errChan <- err
		return
	}
	defer body.Close()

	if err := decodeEvents(body, func(event events.Message, err error) error {
		if err != nil {
			return err
		}
		eventChan <- event
		return nil
	}); err != nil {
		errChan <- err
		return
	}
}

func MonitorWithHandler(
	ctx context.Context,
	cli dockerApi.APIClient,
	options types.EventsOptions,
	handler *EventHandler) chan error {
	eventChan := make(chan events.Message)
	errChan := make(chan error)
	started := make(chan struct{})

	go handler.Watch(eventChan)
	go monitorEvents(ctx, cli, options, started, eventChan, errChan)

	go func() {
		for {
			select {
			case <-ctx.Done():
				// close(eventChan)
				errChan <- nil
			}
		}
	}()

	<-started
	return errChan
}

var advertiseAddress string
var consulAddress string

func init() {
	flag.StringVar(
		&advertiseAddress,
		"advertise",
		"127.0.0.1",
		"Host IP Address to advertise")
	flag.StringVar(
		&consulAddress,
		"consul",
		"127.0.0.1:8500",
		"Consul Service IP Address")
	flag.Parse()
}

func main() {
	dockerCli, err := dockerApi.NewClient(
		DockerApiHost,
		DockerApiVersion,
		nil,
		map[string]string{"User-Agent": ApplicationName})
	if err != nil {
		panic(err)
	}

	consulConfig := consulApi.DefaultConfig()
	consulConfig.Address = consulAddress
	consulCli, err := consulApi.NewClient(consulConfig)
	if err != nil {
		panic(err)
	}

	dockerNodeInfo, err := dockerCli.Info(context.Background())
	if err != nil {
		panic(err)
	}

	log.Println(dockerNodeInfo.Name)

	eventHandler := NewHandler(ByAction)
	eventHandler.Handle(StartEvent, func(message events.Message) {

		container, err := dockerCli.ContainerInspect(
			context.Background(),
			message.ID)
		if err != nil {
			panic(err)
		}

		publicName := container.Config.Labels[ContainerPublicNameLabel]
		if publicName == "" {
			return
		}

		publicPort, err := nat.NewPort(
			"tcp",
			container.Config.Labels[ContainerPublicPortLabel])
		if err != nil {
			fmt.Sprintf(
				"Container %s with Public Name '%s' must have '&s' label configured \n",
				container.ID,
				publicName,
				ContainerPublicPortLabel)
			return
		}

		var exposedPublicPort int;

		if (container.HostConfig.NetworkMode.IsHost()) {
			exposedPublicPort = publicPort.Int()
		} else {
			containerPublicPort, parseErr := strconv.ParseInt(
				container.NetworkSettings.Ports[publicPort][0].HostPort,
				10,
				0)
			if parseErr != nil {
				return
			}

			exposedPublicPort = int(containerPublicPort)
		}

		registration := new(consulApi.AgentServiceRegistration)
		registration.ID = container.ID
		registration.Name = publicName

		registration.Address = advertiseAddress
		registration.Port = exposedPublicPort

		var i int = 0
		registration.Tags = make([]string, len(container.Config.Labels))
		for key, value := range container.Config.Labels {
			registration.Tags[i] = fmt.Sprintf(
				"%s=%s",
				key,
				value)
			i++
		}

		registration.Check = new(consulApi.AgentServiceCheck)
		registration.Check.HTTP = fmt.Sprintf(
			"http://%s:%d",
			advertiseAddress,
			exposedPublicPort)

		registration.Check.Interval = ConsulServiceCheckInterval

		consulErr := consulCli.Agent().ServiceRegister(registration)

		log.Printf(
			"+ service: %s, %s, %s:%d\n",
			registration.ID,
			registration.Name,
			registration.Address,
			registration.Port)

		if consulErr != nil {
			log.Println(consulErr.Error())
		}
	})
	eventHandler.Handle(DieEvent, func(message events.Message) {
		consulErr := consulCli.Agent().ServiceDeregister(message.ID)

		log.Printf(
			"- service: %s\n",
			message.ID)

		if consulErr != nil {
			log.Println(consulErr.Error())
		}
	})

	context, cancel := context.WithCancel(context.Background())

	errChan := MonitorWithHandler(
		context,
		dockerCli,
		types.EventsOptions{},
		eventHandler)

	if err := <-errChan; err != nil {
		cancel()
		panic(err)
	}
}
