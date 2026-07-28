// Code generated manually for MuninnDB. DO NOT EDIT.
// This file provides gRPC service interfaces for MuninnDB.

package muninn_v1

import (
	"context"

	"google.golang.org/grpc"
)

// MuninnDBClient is the client API for MuninnDB service.
type MuninnDBClient interface {
	Hello(ctx context.Context, in *HelloRequest, opts ...grpc.CallOption) (*HelloResponse, error)
	Write(ctx context.Context, in *WriteRequest, opts ...grpc.CallOption) (*WriteResponse, error)
	BatchWrite(ctx context.Context, in *BatchWriteRequest, opts ...grpc.CallOption) (*BatchWriteResponse, error)
	Read(ctx context.Context, in *ReadRequest, opts ...grpc.CallOption) (*ReadResponse, error)
	Forget(ctx context.Context, in *ForgetRequest, opts ...grpc.CallOption) (*ForgetResponse, error)
	BatchForget(ctx context.Context, in *BatchForgetRequest, opts ...grpc.CallOption) (*BatchForgetResponse, error)
	Stat(ctx context.Context, in *StatRequest, opts ...grpc.CallOption) (*StatResponse, error)
	Link(ctx context.Context, in *LinkRequest, opts ...grpc.CallOption) (*LinkResponse, error)
	ListVaults(ctx context.Context, in *ListVaultsRequest, opts ...grpc.CallOption) (*ListVaultsResponse, error)
	AdjustConfidence(ctx context.Context, in *AdjustConfidenceRequest, opts ...grpc.CallOption) (*AdjustConfidenceResponse, error)
	Activate(ctx context.Context, in *ActivateRequest, opts ...grpc.CallOption) (MuninnDB_ActivateClient, error)
	Subscribe(ctx context.Context, opts ...grpc.CallOption) (MuninnDB_SubscribeClient, error)
}

type muninnDBClient struct {
	cc grpc.ClientConnInterface
}

func NewMuninnDBClient(cc grpc.ClientConnInterface) MuninnDBClient {
	return &muninnDBClient{cc}
}

func (c *muninnDBClient) Hello(ctx context.Context, in *HelloRequest, opts ...grpc.CallOption) (*HelloResponse, error) {
	out := new(HelloResponse)
	err := c.cc.Invoke(ctx, "/muninn.v1.MuninnDB/Hello", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *muninnDBClient) Write(ctx context.Context, in *WriteRequest, opts ...grpc.CallOption) (*WriteResponse, error) {
	out := new(WriteResponse)
	err := c.cc.Invoke(ctx, "/muninn.v1.MuninnDB/Write", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *muninnDBClient) BatchWrite(ctx context.Context, in *BatchWriteRequest, opts ...grpc.CallOption) (*BatchWriteResponse, error) {
	out := new(BatchWriteResponse)
	err := c.cc.Invoke(ctx, "/muninn.v1.MuninnDB/BatchWrite", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *muninnDBClient) Read(ctx context.Context, in *ReadRequest, opts ...grpc.CallOption) (*ReadResponse, error) {
	out := new(ReadResponse)
	err := c.cc.Invoke(ctx, "/muninn.v1.MuninnDB/Read", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *muninnDBClient) Forget(ctx context.Context, in *ForgetRequest, opts ...grpc.CallOption) (*ForgetResponse, error) {
	out := new(ForgetResponse)
	err := c.cc.Invoke(ctx, "/muninn.v1.MuninnDB/Forget", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *muninnDBClient) BatchForget(ctx context.Context, in *BatchForgetRequest, opts ...grpc.CallOption) (*BatchForgetResponse, error) {
	out := new(BatchForgetResponse)
	err := c.cc.Invoke(ctx, "/muninn.v1.MuninnDB/BatchForget", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *muninnDBClient) Stat(ctx context.Context, in *StatRequest, opts ...grpc.CallOption) (*StatResponse, error) {
	out := new(StatResponse)
	err := c.cc.Invoke(ctx, "/muninn.v1.MuninnDB/Stat", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *muninnDBClient) Link(ctx context.Context, in *LinkRequest, opts ...grpc.CallOption) (*LinkResponse, error) {
	out := new(LinkResponse)
	err := c.cc.Invoke(ctx, "/muninn.v1.MuninnDB/Link", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *muninnDBClient) ListVaults(ctx context.Context, in *ListVaultsRequest, opts ...grpc.CallOption) (*ListVaultsResponse, error) {
	out := new(ListVaultsResponse)
	err := c.cc.Invoke(ctx, "/muninn.v1.MuninnDB/ListVaults", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *muninnDBClient) AdjustConfidence(ctx context.Context, in *AdjustConfidenceRequest, opts ...grpc.CallOption) (*AdjustConfidenceResponse, error) {
	out := new(AdjustConfidenceResponse)
	err := c.cc.Invoke(ctx, "/muninn.v1.MuninnDB/AdjustConfidence", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *muninnDBClient) Activate(ctx context.Context, in *ActivateRequest, opts ...grpc.CallOption) (MuninnDB_ActivateClient, error) {
	stream, err := c.cc.NewStream(ctx, &grpc.StreamDesc{StreamName: "Activate"}, "/muninn.v1.MuninnDB/Activate", opts...)
	if err != nil {
		return nil, err
	}
	x := &muninnDBActivateClient{stream}
	if err := x.ClientStream.SendMsg(in); err != nil {
		return nil, err
	}
	if err := x.ClientStream.CloseSend(); err != nil {
		return nil, err
	}
	return x, nil
}

type MuninnDB_ActivateClient interface {
	Recv() (*ActivateResponse, error)
	grpc.ClientStream
}

type muninnDBActivateClient struct {
	grpc.ClientStream
}

func (x *muninnDBActivateClient) Recv() (*ActivateResponse, error) {
	m := new(ActivateResponse)
	if err := x.ClientStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

func (c *muninnDBClient) Subscribe(ctx context.Context, opts ...grpc.CallOption) (MuninnDB_SubscribeClient, error) {
	stream, err := c.cc.NewStream(ctx, &grpc.StreamDesc{StreamName: "Subscribe"}, "/muninn.v1.MuninnDB/Subscribe", opts...)
	if err != nil {
		return nil, err
	}
	x := &muninnDBSubscribeClient{stream}
	return x, nil
}

type MuninnDB_SubscribeClient interface {
	Send(*SubscribeRequest) error
	Recv() (*ActivationPush, error)
	grpc.ClientStream
}

type muninnDBSubscribeClient struct {
	grpc.ClientStream
}

func (x *muninnDBSubscribeClient) Send(m *SubscribeRequest) error {
	return x.ClientStream.SendMsg(m)
}

func (x *muninnDBSubscribeClient) Recv() (*ActivationPush, error) {
	m := new(ActivationPush)
	if err := x.ClientStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

// MuninnDBServer is the server API for MuninnDB service.
type MuninnDBServer interface {
	Hello(context.Context, *HelloRequest) (*HelloResponse, error)
	Write(context.Context, *WriteRequest) (*WriteResponse, error)
	BatchWrite(context.Context, *BatchWriteRequest) (*BatchWriteResponse, error)
	Read(context.Context, *ReadRequest) (*ReadResponse, error)
	Forget(context.Context, *ForgetRequest) (*ForgetResponse, error)
	BatchForget(context.Context, *BatchForgetRequest) (*BatchForgetResponse, error)
	Stat(context.Context, *StatRequest) (*StatResponse, error)
	Link(context.Context, *LinkRequest) (*LinkResponse, error)
	ListVaults(context.Context, *ListVaultsRequest) (*ListVaultsResponse, error)
	AdjustConfidence(context.Context, *AdjustConfidenceRequest) (*AdjustConfidenceResponse, error)
	Activate(*ActivateRequest, MuninnDB_ActivateServer) error
	Subscribe(MuninnDB_SubscribeServer) error
}

type UnimplementedMuninnDBServer struct {
}

func (UnimplementedMuninnDBServer) Hello(context.Context, *HelloRequest) (*HelloResponse, error) {
	return nil, nil
}

func (UnimplementedMuninnDBServer) Write(context.Context, *WriteRequest) (*WriteResponse, error) {
	return nil, nil
}

func (UnimplementedMuninnDBServer) BatchWrite(context.Context, *BatchWriteRequest) (*BatchWriteResponse, error) {
	return nil, nil
}

func (UnimplementedMuninnDBServer) Read(context.Context, *ReadRequest) (*ReadResponse, error) {
	return nil, nil
}

func (UnimplementedMuninnDBServer) Forget(context.Context, *ForgetRequest) (*ForgetResponse, error) {
	return nil, nil
}

func (UnimplementedMuninnDBServer) BatchForget(context.Context, *BatchForgetRequest) (*BatchForgetResponse, error) {
	return nil, nil
}

func (UnimplementedMuninnDBServer) Stat(context.Context, *StatRequest) (*StatResponse, error) {
	return nil, nil
}

func (UnimplementedMuninnDBServer) Link(context.Context, *LinkRequest) (*LinkResponse, error) {
	return nil, nil
}

func (UnimplementedMuninnDBServer) ListVaults(context.Context, *ListVaultsRequest) (*ListVaultsResponse, error) {
	return nil, nil
}

func (UnimplementedMuninnDBServer) AdjustConfidence(context.Context, *AdjustConfidenceRequest) (*AdjustConfidenceResponse, error) {
	return nil, nil
}

func (UnimplementedMuninnDBServer) Activate(*ActivateRequest, MuninnDB_ActivateServer) error {
	return nil
}

func (UnimplementedMuninnDBServer) Subscribe(MuninnDB_SubscribeServer) error {
	return nil
}

// MuninnDB_ActivateServer is the server API for MuninnDB.Activate
type MuninnDB_ActivateServer interface {
	Send(*ActivateResponse) error
	grpc.ServerStream
}

// MuninnDB_SubscribeServer is the server API for MuninnDB.Subscribe
type MuninnDB_SubscribeServer interface {
	Send(*ActivationPush) error
	Recv() (*SubscribeRequest, error)
	grpc.ServerStream
}

// RegisterMuninnDBServer registers the MuninnDB server.
func RegisterMuninnDBServer(s *grpc.Server, srv MuninnDBServer) {
	s.RegisterService(&MuninnDB_ServiceDesc, srv)
}

// MuninnDB_ServiceDesc describes the MuninnDB service.
var MuninnDB_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "muninn.v1.MuninnDB",
	HandlerType: (*MuninnDBServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "Hello",
			Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
				in := new(HelloRequest)
				if err := dec(in); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return srv.(MuninnDBServer).Hello(ctx, in)
				}
				info := &grpc.UnaryServerInfo{
					Server:     srv,
					FullMethod: "/muninn.v1.MuninnDB/Hello",
				}
				handler := func(ctx context.Context, req interface{}) (interface{}, error) {
					return srv.(MuninnDBServer).Hello(ctx, req.(*HelloRequest))
				}
				return interceptor(ctx, in, info, handler)
			},
		},
		{
			MethodName: "Write",
			Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
				in := new(WriteRequest)
				if err := dec(in); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return srv.(MuninnDBServer).Write(ctx, in)
				}
				info := &grpc.UnaryServerInfo{
					Server:     srv,
					FullMethod: "/muninn.v1.MuninnDB/Write",
				}
				handler := func(ctx context.Context, req interface{}) (interface{}, error) {
					return srv.(MuninnDBServer).Write(ctx, req.(*WriteRequest))
				}
				return interceptor(ctx, in, info, handler)
			},
		},
		{
			MethodName: "BatchWrite",
			Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
				in := new(BatchWriteRequest)
				if err := dec(in); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return srv.(MuninnDBServer).BatchWrite(ctx, in)
				}
				info := &grpc.UnaryServerInfo{
					Server:     srv,
					FullMethod: "/muninn.v1.MuninnDB/BatchWrite",
				}
				handler := func(ctx context.Context, req interface{}) (interface{}, error) {
					return srv.(MuninnDBServer).BatchWrite(ctx, req.(*BatchWriteRequest))
				}
				return interceptor(ctx, in, info, handler)
			},
		},
		{
			MethodName: "Read",
			Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
				in := new(ReadRequest)
				if err := dec(in); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return srv.(MuninnDBServer).Read(ctx, in)
				}
				info := &grpc.UnaryServerInfo{
					Server:     srv,
					FullMethod: "/muninn.v1.MuninnDB/Read",
				}
				handler := func(ctx context.Context, req interface{}) (interface{}, error) {
					return srv.(MuninnDBServer).Read(ctx, req.(*ReadRequest))
				}
				return interceptor(ctx, in, info, handler)
			},
		},
		{
			MethodName: "Forget",
			Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
				in := new(ForgetRequest)
				if err := dec(in); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return srv.(MuninnDBServer).Forget(ctx, in)
				}
				info := &grpc.UnaryServerInfo{
					Server:     srv,
					FullMethod: "/muninn.v1.MuninnDB/Forget",
				}
				handler := func(ctx context.Context, req interface{}) (interface{}, error) {
					return srv.(MuninnDBServer).Forget(ctx, req.(*ForgetRequest))
				}
				return interceptor(ctx, in, info, handler)
			},
		},
		{
			MethodName: "BatchForget",
			Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
				in := new(BatchForgetRequest)
				if err := dec(in); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return srv.(MuninnDBServer).BatchForget(ctx, in)
				}
				info := &grpc.UnaryServerInfo{
					Server:     srv,
					FullMethod: "/muninn.v1.MuninnDB/BatchForget",
				}
				handler := func(ctx context.Context, req interface{}) (interface{}, error) {
					return srv.(MuninnDBServer).BatchForget(ctx, req.(*BatchForgetRequest))
				}
				return interceptor(ctx, in, info, handler)
			},
		},
		{
			MethodName: "Stat",
			Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
				in := new(StatRequest)
				if err := dec(in); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return srv.(MuninnDBServer).Stat(ctx, in)
				}
				info := &grpc.UnaryServerInfo{
					Server:     srv,
					FullMethod: "/muninn.v1.MuninnDB/Stat",
				}
				handler := func(ctx context.Context, req interface{}) (interface{}, error) {
					return srv.(MuninnDBServer).Stat(ctx, req.(*StatRequest))
				}
				return interceptor(ctx, in, info, handler)
			},
		},
		{
			MethodName: "Link",
			Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
				in := new(LinkRequest)
				if err := dec(in); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return srv.(MuninnDBServer).Link(ctx, in)
				}
				info := &grpc.UnaryServerInfo{
					Server:     srv,
					FullMethod: "/muninn.v1.MuninnDB/Link",
				}
				handler := func(ctx context.Context, req interface{}) (interface{}, error) {
					return srv.(MuninnDBServer).Link(ctx, req.(*LinkRequest))
				}
				return interceptor(ctx, in, info, handler)
			},
		},
		{
			MethodName: "ListVaults",
			Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
				in := new(ListVaultsRequest)
				if err := dec(in); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return srv.(MuninnDBServer).ListVaults(ctx, in)
				}
				info := &grpc.UnaryServerInfo{
					Server:     srv,
					FullMethod: "/muninn.v1.MuninnDB/ListVaults",
				}
				handler := func(ctx context.Context, req interface{}) (interface{}, error) {
					return srv.(MuninnDBServer).ListVaults(ctx, req.(*ListVaultsRequest))
				}
				return interceptor(ctx, in, info, handler)
			},
		},
		{
			MethodName: "AdjustConfidence",
			Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
				in := new(AdjustConfidenceRequest)
				if err := dec(in); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return srv.(MuninnDBServer).AdjustConfidence(ctx, in)
				}
				info := &grpc.UnaryServerInfo{
					Server:     srv,
					FullMethod: "/muninn.v1.MuninnDB/AdjustConfidence",
				}
				handler := func(ctx context.Context, req interface{}) (interface{}, error) {
					return srv.(MuninnDBServer).AdjustConfidence(ctx, req.(*AdjustConfidenceRequest))
				}
				return interceptor(ctx, in, info, handler)
			},
		},
	},
	Streams: []grpc.StreamDesc{
		{
			StreamName:    "Activate",
			Handler:       _MuninnDB_Activate_Handler,
			ServerStreams: true,
		},
		{
			StreamName:    "Subscribe",
			Handler:       _MuninnDB_Subscribe_Handler,
			ServerStreams: true,
			ClientStreams: true,
		},
	},
	Metadata: "muninn/v1/service.proto",
}

func _MuninnDB_Activate_Handler(srv interface{}, stream grpc.ServerStream) error {
	m := new(ActivateRequest)
	if err := stream.RecvMsg(m); err != nil {
		return err
	}
	return srv.(MuninnDBServer).Activate(m, &muninnDBActivateServer{stream})
}

type muninnDBActivateServer struct {
	grpc.ServerStream
}

func (x *muninnDBActivateServer) Send(m *ActivateResponse) error {
	return x.ServerStream.SendMsg(m)
}

func _MuninnDB_Subscribe_Handler(srv interface{}, stream grpc.ServerStream) error {
	return srv.(MuninnDBServer).Subscribe(&muninnDBSubscribeServer{stream})
}

type muninnDBSubscribeServer struct {
	grpc.ServerStream
}

func (x *muninnDBSubscribeServer) Send(m *ActivationPush) error {
	return x.ServerStream.SendMsg(m)
}

func (x *muninnDBSubscribeServer) Recv() (*SubscribeRequest, error) {
	m := new(SubscribeRequest)
	if err := x.ServerStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}
