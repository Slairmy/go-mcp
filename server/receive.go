package server

import (
	"context"
	"errors"
	"fmt"

	"go-mcp/pkg"
	"go-mcp/protocol"

	"github.com/tidwall/gjson"
)

func (server *Server) Receive(ctx context.Context, sessionID string, msg []byte) error {
	ctx = setSessionIDToCtx(ctx, sessionID)

	if !gjson.GetBytes(msg, "id").Exists() {
		notify := &protocol.JSONRPCNotification{}
		if err := pkg.JsonUnmarshal(msg, &notify); err != nil {
			return err
		}
		go func() {
			defer pkg.Recover()

			if err := server.receiveNotify(sessionID, notify); err != nil {
				server.logger.Errorf("receive notify:%+v error: %s", notify, err.Error())
				return
			}
		}()
		return nil
	}

	// 判断 request和response
	if !gjson.GetBytes(msg, "method").Exists() {
		resp := &protocol.JSONRPCResponse{}
		if err := pkg.JsonUnmarshal(msg, &resp); err != nil {
			return err
		}
		go func() {
			defer pkg.Recover()

			if err := server.receiveResponse(sessionID, resp); err != nil {
				server.logger.Errorf("receive response:%+v error: %s", resp, err.Error())
				return
			}
		}()
		return nil
	}

	req := &protocol.JSONRPCRequest{}
	if err := pkg.JsonUnmarshal(msg, &req); err != nil {
		return err
	}
	if !req.IsValid() {
		return pkg.ErrRequestInvalid
	}
	server.inFlyRequest.Add(1)
	if server.inShutdown.Load() {
		defer server.inFlyRequest.Done()
		return errors.New("server already shutdown")
	}
	go func() {
		defer pkg.Recover()
		defer server.inFlyRequest.Done()

		if err := server.receiveRequest(ctx, sessionID, req); err != nil {
			server.logger.Errorf("receive request:%+v error: %s", req, err.Error())
			return
		}
	}()

	return nil
}

func (server *Server) receiveRequest(ctx context.Context, sessionID string, request *protocol.JSONRPCRequest) error {
	if request.Method != protocol.Initialize && request.Method != protocol.Ping {
		if s, ok := server.sessionID2session.Load(sessionID); !ok {
			return pkg.ErrLackSession
		} else if !s.ready.Load() {
			return pkg.ErrSessionHasNotInitialized
		}
	}

	var (
		result protocol.ServerResponse
		err    error
	)

	switch request.Method {
	case protocol.Ping:
		result, err = server.handleRequestWithPing()
	case protocol.Initialize:
		result, err = server.handleRequestWithInitialize(sessionID, request.RawParams)
	case protocol.PromptsList:
		result, err = server.handleRequestWithListPrompts(request.RawParams)
	case protocol.PromptsGet:
		result, err = server.handleRequestWithGetPrompt(request.RawParams)
	case protocol.ResourcesList:
		result, err = server.handleRequestWithListResources(request.RawParams)
	case protocol.ResourceListTemplates:
		result, err = server.handleRequestWithListResourceTemplates(request.RawParams)
	case protocol.ResourcesRead:
		result, err = server.handleRequestWithReadResource(request.RawParams)
	case protocol.ResourcesSubscribe:
		result, err = server.handleRequestWithSubscribeResourceChange(sessionID, request.RawParams)
	case protocol.ResourcesUnsubscribe:
		result, err = server.handleRequestWithUnSubscribeResourceChange(sessionID, request.RawParams)
	case protocol.ToolsList:
		result, err = server.handleRequestWithListTools(request.RawParams)
	case protocol.ToolsCall:
		result, err = server.handleRequestWithCallTool(request.RawParams)
	default:
		err = fmt.Errorf("%w: method=%s", pkg.ErrMethodNotSupport, request.Method)
	}

	if err != nil {
		if errors.Is(err, pkg.ErrMethodNotSupport) {
			return server.sendMsgWithError(ctx, sessionID, request.ID, protocol.METHOD_NOT_FOUND, err.Error())
		} else if errors.Is(err, pkg.ErrRequestInvalid) {
			return server.sendMsgWithError(ctx, sessionID, request.ID, protocol.INVALID_REQUEST, err.Error())
		} else if errors.Is(err, pkg.ErrJsonUnmarshal) {
			return server.sendMsgWithError(ctx, sessionID, request.ID, protocol.PARSE_ERROR, err.Error())
		}
		return server.sendMsgWithError(ctx, sessionID, request.ID, protocol.INTERNAL_ERROR, err.Error())
	}
	return server.sendMsgWithResponse(ctx, sessionID, request.ID, result)
}

func (server *Server) receiveNotify(sessionID string, notify *protocol.JSONRPCNotification) error {
	if s, ok := server.sessionID2session.Load(sessionID); !ok {
		return pkg.ErrLackSession
	} else if !s.ready.Load() && notify.Method != protocol.NotificationInitialized {
		return pkg.ErrSessionHasNotInitialized
	}

	switch notify.Method {
	case protocol.NotificationInitialized:
		return server.handleNotifyWithInitialized(sessionID, notify.RawParams)
	default:
		return fmt.Errorf("%w: method=%s", pkg.ErrMethodNotSupport, notify.Method)
	}
}

func (server *Server) receiveResponse(sessionID string, response *protocol.JSONRPCResponse) error {
	s, ok := server.sessionID2session.Load(sessionID)
	if !ok {
		return pkg.ErrLackSession
	}
	if !s.ready.Load() {
		return pkg.ErrSessionHasNotInitialized
	}

	respChan, ok := s.reqID2respChan.Get(fmt.Sprint(response.ID))
	if !ok {
		return fmt.Errorf("%w: sessionID=%+v, requestID=%+v", pkg.ErrLackResponseChan, sessionID, response.ID)
	}

	select {
	case respChan <- response:
	default:
		return fmt.Errorf("%w: sessionID=%+v, response=%+v", pkg.ErrDuplicateResponseReceived, sessionID, response)
	}
	return nil
}
