package predictor

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	guuid "github.com/google/uuid"
	"github.com/seldonio/seldon-core/executor/api/client"
	"github.com/seldonio/seldon-core/executor/api/payload"
	payloadLogger "github.com/seldonio/seldon-core/executor/logger"
	"github.com/seldonio/seldon-core/operator/apis/machinelearning/v1"
	"net/url"
	"sync"
)

type PredictorProcess struct {
	Ctx       context.Context
	Client    client.SeldonApiClient
	Log       logr.Logger
	ServerUrl *url.URL
	Namespace string
}

func NewPredictorProcess(context context.Context, client client.SeldonApiClient, log logr.Logger, serverUrl *url.URL, namespace string) PredictorProcess {
	return PredictorProcess{
		Ctx:       context,
		Client:    client,
		Log:       log,
		ServerUrl: serverUrl,
		Namespace: namespace,
	}
}

func hasMethod(method v1.PredictiveUnitMethod, methods *[]v1.PredictiveUnitMethod) bool {
	if methods != nil {
		for _, m := range *methods {
			if m == method {
				return true
			}
		}
	}
	return false
}

func (p *PredictorProcess) transformInput(node *v1.PredictiveUnit, msg payload.SeldonPayload) (payload.SeldonPayload, error) {
	callModel := false
	callTransformInput := false
	if (*node).Type != nil {
		switch *node.Type {
		case v1.MODEL:
			callModel = true
		case v1.TRANSFORMER:
			callTransformInput = true
		}
	}
	if hasMethod(v1.TRANSFORM_INPUT, node.Methods) {
		callTransformInput = true
	}
	if callModel {
		msg, err := p.Client.Chain(p.Ctx, node.Name, msg)
		if err != nil {
			return nil, err
		}
		return p.Client.Predict(p.Ctx, node.Name, node.Endpoint.ServiceHost, node.Endpoint.ServicePort, msg)
	} else if callTransformInput {
		msg, err := p.Client.Chain(p.Ctx, node.Name, msg)
		if err != nil {
			return nil, err
		}
		return p.Client.TransformInput(p.Ctx, node.Name, node.Endpoint.ServiceHost, node.Endpoint.ServicePort, msg)
	} else {
		return msg, nil
	}

}

func (p *PredictorProcess) transformOutput(node *v1.PredictiveUnit, msg payload.SeldonPayload) (payload.SeldonPayload, error) {
	callClient := false
	if (*node).Type != nil {
		switch *node.Type {
		case v1.OUTPUT_TRANSFORMER:
			callClient = true
		}
	}
	if hasMethod(v1.TRANSFORM_OUTPUT, node.Methods) {
		callClient = true
	}

	if callClient {
		msg, err := p.Client.Chain(p.Ctx, node.Name, msg)
		if err != nil {
			return nil, err
		}
		return p.Client.TransformOutput(p.Ctx, node.Name, node.Endpoint.ServiceHost, node.Endpoint.ServicePort, msg)
	} else {
		return msg, nil
	}

}

func (p *PredictorProcess) feedback(node *v1.PredictiveUnit, msg payload.SeldonPayload) (payload.SeldonPayload, error) {
	callClient := false
	if (*node).Type != nil {
		switch *node.Type {
		case v1.MODEL:
			callClient = true
		}
	}
	if hasMethod(v1.SEND_FEEDBACK, node.Methods) {
		callClient = true
	}

	if callClient {
		return p.Client.Feedback(p.Ctx, node.Name, node.Endpoint.ServiceHost, node.Endpoint.ServicePort, msg)
	} else {
		return msg, nil
	}

}

func (p *PredictorProcess) route(node *v1.PredictiveUnit, msg payload.SeldonPayload) (int, error) {
	callClient := false
	if (*node).Type != nil {
		switch *node.Type {
		case v1.ROUTER:
			callClient = true
		}
	}
	if hasMethod(v1.ROUTE, node.Methods) {
		callClient = true
	}
	if callClient {
		return p.Client.Route(p.Ctx, node.Name, node.Endpoint.ServiceHost, node.Endpoint.ServicePort, msg)
	} else if node.Implementation != nil && *node.Implementation == v1.RANDOM_ABTEST {
		return p.abTestRouter(node)
	} else {
		return -1, nil
	}
}

func (p *PredictorProcess) aggregate(node *v1.PredictiveUnit, msg []payload.SeldonPayload) (payload.SeldonPayload, error) {
	callClient := false
	if (*node).Type != nil {
		switch *node.Type {
		case v1.COMBINER:
			callClient = true
		}
	}
	if hasMethod(v1.AGGREGATE, node.Methods) {
		callClient = true
	}

	if callClient {
		return p.Client.Combine(p.Ctx, node.Name, node.Endpoint.ServiceHost, node.Endpoint.ServicePort, msg)
	} else {
		return msg[0], nil
	}

}

func (p *PredictorProcess) predictChildren(node *v1.PredictiveUnit, msg payload.SeldonPayload) (payload.SeldonPayload, error) {
	if node.Children != nil && len(node.Children) > 0 {
		route, err := p.route(node, msg)
		if err != nil {
			return nil, err
		}
		var cmsgs []payload.SeldonPayload
		if route == -1 {
			cmsgs = make([]payload.SeldonPayload, len(node.Children))
			var errs = make([]error, len(node.Children))
			wg := sync.WaitGroup{}
			for i, nodeChild := range node.Children {
				wg.Add(1)
				go func(i int, nodeChild v1.PredictiveUnit, msg payload.SeldonPayload) {
					cmsgs[i], errs[i] = p.Predict(&nodeChild, msg)
					wg.Done()
				}(i, nodeChild, msg)
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					return cmsgs[i], err
				}
			}
		} else {
			cmsgs = make([]payload.SeldonPayload, 1)
			cmsgs[0], err = p.Predict(&node.Children[route], msg)
			if err != nil {
				return cmsgs[0], err
			}
		}
		return p.aggregate(node, cmsgs)
	} else {
		return msg, nil
	}
}

func (p *PredictorProcess) feedbackChildren(node *v1.PredictiveUnit, msg payload.SeldonPayload) (payload.SeldonPayload, error) {
	if node.Children != nil && len(node.Children) > 0 {
		route, err := p.route(node, msg)
		if err != nil {
			return nil, err
		}
		var cmsgs []payload.SeldonPayload
		if route == -1 {
			cmsgs = make([]payload.SeldonPayload, len(node.Children))
			var errs = make([]error, len(node.Children))
			wg := sync.WaitGroup{}
			for i, nodeChild := range node.Children {
				wg.Add(1)
				go func(i int, nodeChild v1.PredictiveUnit, msg payload.SeldonPayload) {
					cmsgs[i], errs[i] = p.Feedback(&nodeChild, msg)
					wg.Done()
				}(i, nodeChild, msg)
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					return cmsgs[i], err
				}
			}
		} else {
			cmsgs = make([]payload.SeldonPayload, 1)
			cmsgs[0], err = p.Feedback(&node.Children[route], msg)
			if err != nil {
				return cmsgs[0], err
			}
		}
		return p.aggregate(node, cmsgs)
	} else {
		return msg, nil
	}
}

func (p *PredictorProcess) getLogUrl(logger *v1.Logger) (*url.URL, error) {
	if logger.Url != nil {
		return url.Parse(*logger.Url)
	} else {
		return url.Parse(payloadLogger.GetLoggerDefaultUrl(p.Namespace))
	}
}

func (p *PredictorProcess) logPayload(nodeName string, logger *v1.Logger, reqType payloadLogger.LogRequestType, msg payload.SeldonPayload) error {
	payload, err := msg.GetBytes()
	if err != nil {
		return err
	}
	logUrl, err := p.getLogUrl(logger)
	if err != nil {
		return err
	}

	payloadLogger.QueueLogRequest(payloadLogger.LogRequest{
		Url:         logUrl,
		Bytes:       &payload,
		ContentType: msg.GetContentType(),
		ReqType:     reqType,
		Id:          guuid.New().String(),
		SourceUri:   p.ServerUrl,
		ModelId:     nodeName,
	})
	return nil
}

func (p *PredictorProcess) Predict(node *v1.PredictiveUnit, msg payload.SeldonPayload) (payload.SeldonPayload, error) {
	//Log Request
	if node.Logger != nil && (node.Logger.Mode == v1.LogRequest || node.Logger.Mode == v1.LogAll) {
		p.logPayload(node.Name, node.Logger, payloadLogger.InferenceRequest, msg)
	}
	tmsg, err := p.transformInput(node, msg)
	if err != nil {
		return tmsg, err
	}
	cmsg, err := p.predictChildren(node, tmsg)
	if err != nil {
		return tmsg, err
	}
	response, err := p.transformOutput(node, cmsg)
	// Log Response
	if err == nil && node.Logger != nil && (node.Logger.Mode == v1.LogResponse || node.Logger.Mode == v1.LogAll) {
		p.logPayload(node.Name, node.Logger, payloadLogger.InferenceResponse, response)
	}
	return response, err
}

func (p *PredictorProcess) Status(node *v1.PredictiveUnit, modelName string, msg payload.SeldonPayload) (payload.SeldonPayload, error) {
	if nodeModel := v1.GetPredictiveUnit(node, modelName); nodeModel == nil {
		return nil, fmt.Errorf("Failed to find model %s", modelName)
	} else {
		return p.Client.Status(p.Ctx, modelName, nodeModel.Endpoint.ServiceHost, nodeModel.Endpoint.ServicePort, msg)
	}
}

func (p *PredictorProcess) Metadata(node *v1.PredictiveUnit, modelName string, msg payload.SeldonPayload) (payload.SeldonPayload, error) {
	if nodeModel := v1.GetPredictiveUnit(node, modelName); nodeModel == nil {
		return nil, fmt.Errorf("Failed to find model %s", modelName)
	} else {
		return p.Client.Metadata(p.Ctx, modelName, nodeModel.Endpoint.ServiceHost, nodeModel.Endpoint.ServicePort, msg)
	}
}

func (p *PredictorProcess) Feedback(node *v1.PredictiveUnit, msg payload.SeldonPayload) (payload.SeldonPayload, error) {
	tmsg, err := p.feedbackChildren(node, msg)
	if err != nil {
		return tmsg, err
	}
	return p.feedback(node, msg)
}
