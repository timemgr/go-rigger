package rigger

import (
	"github.com/AsynkronIT/protoactor-go/actor"
	"github.com/golang/protobuf/proto"
	log "github.com/sirupsen/logrus"
	"reflect"
	"time"
)

/**
通用服务器
 */

// 通用服务器行为模式
type GeneralServerBehaviour interface {
	// 启动时的回调,应该在此回调中进行初始化,不管是正常启动或是重启,都会调用此事件
	LifeCyclePart
	// 结果需要返回给请求进程,为了保证能够跨节点,需要是proto.Message
	OnMessage(ctx actor.Context, message interface{}) proto.Message
	//OnCast(ctx actor.Context, msg interface{})
	//OnCall(ctx actor.Context, msg interface{})
	//PidHolder
}

// 行为模式生成器
type GeneralServerBehaviourProducer func() GeneralServerBehaviour

func StartGeneralServer(parent interface{}, id string) (*GeneralServer, error) {
	if _, ok := getRegisterInfo(id); ok {
		server := NewGeneralServer()
		_, err := server.WithSupervisor(parent).WithSpawner(parent).StartSpec(&SpawnSpec{
			Id: id,
			SpawnTimeout: startTimeOut,
		})
		if err != nil {
			return nil, err
		}
		return server, nil
	} else {
		return nil, ErrNotRegister(id)
	}
}

func StartGeneralServerSpec(parent interface{}, spec *SpawnSpec) (*GeneralServer, error) {
	server := NewGeneralServer()
	return server.WithSupervisor(parent).WithSpawner(parent).StartSpec(spec)
}

// 生成一个新的GeneralServer
func NewGeneralServer() *GeneralServer  {
	server := &GeneralServer{}
	return server
}

// 通用服务器
type GeneralServer struct {
	pid *actor.PID // 进程ID
	spawner actor.SpawnerContext
	strategy actor.SupervisorStrategy
	initArgs interface{} // 初始化参数
	delegate *genServerDelegate
	receiveTimeout time.Duration
}

// 添加监控,需要在Start之前执行,并且只能设置一次非空supervisor,如果重复设置,则简单忽略
func (server *GeneralServer)WithSupervisor(maybeSupervisor interface{}) *GeneralServer  {
	withSupervisor(server, maybeSupervisor)
	return server
}

func (server *GeneralServer)WithRawSupervisor(strategy actor.SupervisorStrategy) *GeneralServer  {
	server.strategy = strategy
	return server
}

func (server *GeneralServer)WithSpawner(spawner interface{}) *GeneralServer {
	withSpawner(server, spawner)
	return server
}

// 使用启动规范启动一个Actor
func (server *GeneralServer)StartSpec(spec *SpawnSpec) (*GeneralServer, error){
	if info, ok := getRegisterInfo(spec.Id); ok {
		switch prod := info.producer.(type) {
		case GeneralServerBehaviourProducer:
			props, initFuture := server.prepareSpawn(prod, spec.SpawnTimeout)
			// 检查startFun
			startFun := makeStartFun(info)
			if pid, err := startFun(server.spawner, props, spec.Args); err != nil {
				log.Errorf("error when start actor, reason:%s", err.Error())
				return server, err
			} else {
				// 在启动完成前设置启动参数
				server.initArgs = spec.Args
				// 设置receiveTimeout
				if spec.ReceiveTimeout >= 0 {
					server.receiveTimeout = spec.ReceiveTimeout
				} else {
					server.receiveTimeout = -1
				}
				// 等待
				if initFuture != nil {
					if err = initFuture.Wait(); err != nil {
						log.Errorf("error when wait start actor reason:%s", err)
						return server, err
					}
				}
				server.pid = pid
			}
		default:
			return server, ErrWrongProducer(reflect.TypeOf(prod).Name())
		}
		return server, nil

	} else {
		return nil, ErrNotRegister(spec.Id)
	}
}

// Interface: Stoppable
func (server *GeneralServer) Stop() {
	server.spawner.ActorSystem().Root.Stop(server.pid)
}

func (server *GeneralServer) StopFuture() *actor.Future {
	return server.spawner.ActorSystem().Root.StopFuture(server.pid)
}

func (server *GeneralServer) Poison() {
	server.spawner.ActorSystem().Root.Poison(server.pid)
}

func (server *GeneralServer) PoisonFuture() *actor.Future {
	return server.spawner.ActorSystem().Root.PoisonFuture(server.pid)
}

// Interface: Sender
// 给genser发送一条消息
func (server *GeneralServer)Send(sender actor.SenderContext, msg interface{})  {
	sender.Send(server.pid, msg)
}

func (server *GeneralServer)Request(sender actor.SenderContext, msg interface{})  {
	sender.Request(server.pid, msg)
}

// 默认5秒超时
func (server *GeneralServer)RequestFutureDefault(sender actor.SenderContext, msg interface{}) *actor.Future {
	return server.RequestFuture(sender, msg, 5000000000)
}

func (server *GeneralServer)RequestFuture(sender actor.SenderContext, msg interface{}, timeout time.Duration) *actor.Future {
	return sender.RequestFuture(server.pid, msg, timeout)
}

func (server *GeneralServer) generateProps(producer GeneralServerBehaviourProducer, future *actor.Future) *actor.Props  {
	props := actor.PropsFromProducer(func() actor.Actor {
		return &genServerDelegate{
			initFuture: future,
			callback: producer(),
			owner: server,
		}
	})

	// 是否需要监控
	if server.strategy != nil {
		props.WithSupervisor(server.strategy)
	}

	return props
}

func (server *GeneralServer) prepareSpawn(producer GeneralServerBehaviourProducer, timeout time.Duration) (*actor.Props, *actor.Future) {
	if server.spawner == nil {
		server.WithSpawner(actor.NewActorSystem().Root)
	}
	var initFuture *actor.Future
	if timeout <= 0 {
		initFuture = nil
	} else {
		initFuture = actor.NewFuture(server.spawner.ActorSystem(), timeout)
	}
	props := server.generateProps(producer, initFuture)


	return props, initFuture
}

// Interface: SpawnerSetter
func (server *GeneralServer) setSpawner(spawner actor.SpawnerContext) spawnerSetter {
	server.spawner = spawner
	return server
}

// Interface: supervisorSetter
func (server *GeneralServer) setSupervisor(strategy actor.SupervisorStrategy) supervisorSetter {
	server.strategy = strategy
	return server
}


// GeneralServer代理
type genServerDelegate struct {
	callback GeneralServerBehaviour
	initFuture *actor.Future // 初始化future,如果设置了,初始化完成后通过future进行通知
	owner *GeneralServer
	context actor.Context
}

func (server *genServerDelegate) Receive(context actor.Context) {
	switch msg := context.Message().(type) {
	case *actor.Started:
		// TODO 如果是这里通知的应该是有异常了
		defer server.notifyIninComplete(context)

		// 设置GeneralServer中的代理指针
		server.owner.delegate = server
		//server.callback.SetPid(context.RespondSelf())
		// 设置超时
		if server.owner.receiveTimeout > 0 {
			context.SetReceiveTimeout(server.owner.receiveTimeout)
		}
		server.callback.OnStarted(context, server.owner.initArgs)
		// 初始化完成了,通知后,继续进行后面的初始化
		server.notifyIninComplete(context)
		server.callback.OnPostStarted(context, server.owner.initArgs)
		server.context = context
	case *actor.Restarting:
		server.callback.OnRestarting(context)
	case *actor.Stopping:
		server.callback.OnStopping(context)
	case *actor.Stopped:
		server.callback.OnStopped(context)
		server.owner.delegate = nil
		server.owner = nil
		server.context = nil
	case *actor.ReceiveTimeout:
		re := server.callback.(TimeoutReceiver)
		re.OnTimeout(context)
	default:
		ret := server.callback.OnMessage(context, msg)
		if ret != NoReply {
			switch r := ret.(type) {
			case *Forward:
				server.forward(context, r)
			default:
				if context.Sender() != nil {
					context.Respond(ret)
				}
			}
		}
	}
}

func (server *genServerDelegate)notifyIninComplete(context actor.Context)  {
	if server.initFuture != nil {
		context.Send(server.initFuture.PID(), context.Self())
		server.initFuture = nil
	}
}

func (server *genServerDelegate) forward(context actor.Context, forwardInfo *Forward)  {
	if forwardInfo.To == nil {
		return
	}
	switch forwardInfo.RespondType {
	case RespondNone:
		context.Send(forwardInfo.To, forwardInfo.Message)
	case RespondOrigin: // 回复给原始进程
		context.RequestWithCustomSender(forwardInfo.To, forwardInfo.Message, context.Sender())
	case RespondSelf:
		context.Request(forwardInfo.To, forwardInfo.Message)
	}
}
