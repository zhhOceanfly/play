package server

import (
	"errors"
	"fmt"
	"github.com/leochen2038/play"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type runningInstance struct {
	server play.ServerInstance
	listener net.Listener
}

var (
	defaultActionTimeout = 500*time.Millisecond
	instanceWaitGroup sync.WaitGroup
	instances  sync.Map
	ppidkilled bool
)

const (
	envGraceful = "GRACEFUL"
)

const (
	TypeHttp = 1
	TypeTcp = 2
	TypeSse = 3
	TypeWebsocket = 4
)

func init() {
	go signalHandler()
}

type filer interface {
	File() (*os.File, error)
}

func Wait() {
	instanceWaitGroup.Wait()
}

func Boot(i play.ServerInstance) error {
	var err error
	var listener net.Listener
	var gracefulSocket = getGracefulSocket(i.Name())

	if gracefulSocket > 0 {
		if listener, err = net.FileListener(os.NewFile(gracefulSocket, "")); err != nil {
			return err
		}
		if err = shouldKillParent(); err != nil {
			log.Println("[http server] failed to close parent:", err)
			os.Exit(1)
		}
	} else if listener, err = net.Listen("tcp", i.Address()); err != nil {
		return err
	}
	if _, ok := instances.Load(i.Name()); ok {
		_ = listener.Close()
		return errors.New("server name " +i.Name()+" is running")
	}

	instanceWaitGroup.Add(1)
	instances.Store(i.Name(), runningInstance{listener: listener, server: i})
	go func() {
		defer instanceWaitGroup.Done()
		_ = i.Run(listener)
	}()

	return nil
}

func ShutdownAll() {
	instances.Range(func(key, value interface{}) bool {
		run := value.(runningInstance)
		Shutdown(run.server.Name())
		return true
	})
}

func Shutdown(name string) {
	if v, ok := instances.Load(name); ok {
		instances.Delete(name)
		v.(runningInstance).server.Close()
		_ = v.(runningInstance).listener.Close()
	}
}

func doRequest(i play.ServerInstance, s *play.Session, request *play.Request) {
	var action = play.ActionInfo{Name: request.ActionName, Respond: request.Respond, RequestTime: time.Now(), Timeout: defaultActionTimeout, Tag: request.Tag}
	var trace = play.TraceContext{TraceId: request.TraceId, ParentSpanId: request.SpanId, StartTime: time.Now(), ServerName: request.ActionName}

	defer func() {
		if panicInfo := recover(); panicInfo != nil {
			log.Fatal(fmt.Errorf("panic: %v\n%v", panicInfo, string(debug.Stack())))
		}
	}()

	ctx := play.NewContextWithRequest(i, action, request.Parser, trace, s)

	// call custom onRequest
	if ctx.Err = i.OnRequest(ctx); ctx.Err != nil {
		goto RENDER
	}
	ctx.Err = play.RunAction(ctx)

RENDER:
	if i.Render != nil {
		i.Render(ctx)
	}
}

func reload() (int, error) {
	var err error
	var tags []string
	var sockes = make([]*os.File, 0, 1)
	var originalWD, _ = os.Getwd()

	defer func() {
		for _, v := range sockes {
			_ = v.Close()
		}
	}()

	var socketId = 0
	instances.Range(func(key, value interface{}) bool {
		run := value.(runningInstance)
		socket, _ := run.listener.(filer).File()
		sockes = append(sockes, socket)
		tags = append(tags, key.(string) + ":" + strconv.Itoa(socketId))
		socketId++
		return true
	})

	argv0, err := exec.LookPath(os.Args[0])
	if err != nil {
		return 0, err
	}

	var env []string
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, envGraceful) {
			env = append(env, v)
		}
	}

	env = append(env, fmt.Sprintf("%s=%s", envGraceful, strings.Join(tags, "-")))
	files := []*os.File{os.Stdin, os.Stdout, os.Stderr}
	files = append(files, sockes...)

	process, err := os.StartProcess(argv0, os.Args, &os.ProcAttr{
		Dir:   originalWD,
		Env:   env,
		Files: files,
	})
	if err != nil {
		return 0, err
	}
	return process.Pid, nil
}

func signalHandler() {
	ch := make(chan os.Signal, 10)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR2)

	for {
		switch <-ch {
		case syscall.SIGINT, syscall.SIGTERM:
			signal.Stop(ch)
			ShutdownAll()
		case syscall.SIGUSR2:
			if _, err := reload(); err != nil {
				fmt.Println("reload error:", err.Error())
			}
		}
	}
}

func shouldKillParent() (err error) {
	if !ppidkilled && os.Getppid() != 1 {
		ppidkilled = true
		if err := syscall.Kill(os.Getppid(), syscall.SIGTERM); err == nil {
			log.Printf("[play server] graceful handoff success with new pid %d", os.Getpid())
		}
	}
	return
}

func getGracefulSocket(name string) (id uintptr) {
	if os.Getenv(envGraceful) != "" {
		for _, v := range strings.Split(os.Getenv(envGraceful), "-") {
			if socket := strings.Split(v, ":"); len(socket) == 2 {
				if socket[0] == name {
					socketId, _ := strconv.Atoi(socket[1])
					return uintptr(socketId) + 2
				}
			}
		}
	}
	return
}