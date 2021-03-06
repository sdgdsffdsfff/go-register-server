package service

import (
	"encoding/json"
	"errors"
	"github.com/choerodon/go-register-server/pkg/api/entity"
	"github.com/choerodon/go-register-server/pkg/api/metrics"
	"github.com/choerodon/go-register-server/pkg/api/repository"
	"github.com/choerodon/go-register-server/pkg/embed"
	"github.com/choerodon/go-register-server/pkg/k8s"
	"github.com/choerodon/go-register-server/pkg/utils"
	"github.com/emicklei/go-restful"
	"github.com/ghodss/yaml"
	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/go-playground/validator.v9"
	"reflect"
	"strings"
)

type ConfigService interface {
	Save(request *restful.Request, response *restful.Response)
	Poll(request *restful.Request, response *restful.Response)
}

type ConfigServiceImpl struct {
	validate          *validator.Validate
	appRepo           *repository.ApplicationRepository
	configMapOperator k8s.ConfigMapOperator
}

func NewConfigServiceImpl(appRepo *repository.ApplicationRepository) *ConfigServiceImpl {
	s := &ConfigServiceImpl{
		validate:          validator.New(),
		appRepo:           appRepo,
		configMapOperator: k8s.NewConfigMapOperator(),
	}
	_ = s.validate.RegisterValidation("updatePolicy", entity.ValidateUpdatePolicy)
	return s
}

func (es *ConfigServiceImpl) Save(request *restful.Request, response *restful.Response) {
	metrics.RequestCount.With(prometheus.Labels{"path": request.Request.RequestURI}).Inc()
	dto := new(entity.SaveConfigDTO)
	err := request.ReadEntity(&dto)
	if err != nil {
		glog.Warningf("Save config failed when readEntity", err)
		_ = response.WriteErrorString(400, "invalid saveConfigDTO")
		return
	}
	err = es.validate.Struct(dto)
	if err != nil {
		glog.Warningf("Save config failed cause of invalid saveConfigDTO", err)
		_ = response.WriteErrorString(400, "invalid saveConfigDTO")
		return
	}
	source := make(map[string]interface{})
	err = yaml.Unmarshal([]byte(dto.Yaml), &source)
	if err != nil {
		glog.Warningf("Save config failed cause of invalid yaml", err)
		_ = response.WriteErrorString(400, "invalid yaml")
		return
	}

	if dto.Service == entity.ApiGatewayServiceName {
		gb, rb, rm, err := separateRoute(source)
		if err != nil {
			glog.Warningf("Save config failed when separateRoute", err)
			_ = response.WriteErrorString(500, "separateRoute error")
			return
		}
		dto.Yaml = gb
		routeDTO := &entity.SaveConfigDTO{
			Service:      entity.RouteConfigMap,
			Version:      dto.Version,
			Profile:      entity.DefaultProfile,
			Namespace:    dto.Namespace,
			UpdatePolicy: dto.UpdatePolicy,
			Yaml:         rb,
		}
		es.createOrUpdateConfigMap(routeDTO, rm, response)
	}
	es.createOrUpdateConfigMap(dto, source, response)
}

func (es *ConfigServiceImpl) createOrUpdateConfigMap(dto *entity.SaveConfigDTO, source map[string]interface{}, response *restful.Response) {
	queryConfigMap := es.configMapOperator.QueryConfigMap(dto.Service, dto.Namespace)
	if queryConfigMap == nil {
		_, err := es.configMapOperator.CreateConfigMap(dto)
		if err != nil {
			glog.Warningf("Save config failed when create configMap", err)
			_ = response.WriteErrorString(500, "create configMap failed")
			return
		}
	}
	if queryConfigMap != nil && dto.UpdatePolicy == entity.UpdatePolicyNot {
		glog.Infof("configMap %s is already exist", dto.Service)
		_ = response.WriteErrorString(304, "configMap is already exist")
		return
	}

	if dto.UpdatePolicy == entity.UpdatePolicyAdd {
		profileKey := utils.ConfigMapProfileKey(dto.Profile)
		oldYaml := queryConfigMap.Data[profileKey]
		if oldYaml != "" {
			newYaml, err := addProperty(oldYaml, source)
			if err != nil {
				glog.Warningf("Save config failed when merge yaml", err)
				_ = response.WriteErrorString(500, "merge yaml failed")
				return
			}
			dto.Yaml = newYaml
		}
	}
	if dto.UpdatePolicy != entity.UpdatePolicyNot {
		_, err := es.configMapOperator.UpdateConfigMap(dto)
		if err != nil {
			glog.Warningf("Save config failed when update configMap", err)
			_ = response.WriteErrorString(500, "update configMap failed")
			return
		}
	}
}

func (es *ConfigServiceImpl) Poll(request *restful.Request, response *restful.Response) {
	metrics.RequestCount.With(prometheus.Labels{"path": request.Request.RequestURI}).Inc()
	service := request.PathParameter("service")
	if service == "" {
		_ = response.WriteErrorString(400, "service is empty")
		return
	}
	version := request.PathParameter("version")
	if version == "" {
		_ = response.WriteErrorString(400, "version is empty")
		return
	}
	kvMap, configMapVersion, err := es.getConfigFromConfigMap(service, version)
	if err != nil {
		_ = response.WriteErrorString(404, "can't find correct configMap")
		glog.Warningf("Get config from configMap failed, service: %s", service, err)
		return
	}
	if isGateway(service) {
		routeMap, _, err := es.getConfigFromConfigMap(entity.RouteConfigMap, version)
		if err != nil {
			_ = response.WriteErrorString(404, "can't find zuul-route configMap")
			glog.Warningf("Get zuul-route from configMap failed", err)
			return
		}
		// 如果是api-gateway或者gateway-helper，则删除他们配置里的路由配置，添加'zuul-route'configMap里的路由配置
		for k, _ := range kvMap {
			if strings.HasPrefix(k, "zuul.routes.") {
				delete(kvMap, k)
			}
		}
		for k, v := range routeMap {
			kvMap[k] = v
		}
	}
	es.appendConfigServerAddition(kvMap)
	env := &entity.Environment{
		Name:            service,
		Version:         configMapVersion,
		Profiles:        []string{version},
		PropertySources: []entity.PropertySource{{Name: service + "-" + version + "-" + configMapVersion, Source: kvMap}},
	}
	if embed.Env.ConfigServer.Log {
		printConfig, _ := json.MarshalIndent(kvMap, "", "  ")
		glog.Infof("%s-%v pulled config: %s", service, version, printConfig)
	} else {
		glog.Infof("%s-%v pulled config", service, version)
	}
	err = response.WriteAsJson(env)
	if err != nil {
		glog.Warningf("GetConfig write apps.Environment as json error,  msg : %s", env, err)
	}
}

func (es *ConfigServiceImpl) appendConfigServerAddition(kvMap map[string]interface{}) {
	for k, v := range entity.ConfigServerAdditions {
		kvMap[k] = v
	}
}

func (es *ConfigServiceImpl) getConfigFromConfigMap(service string, version string) (map[string]interface{}, string, error) {
	source := make(map[string]interface{})
	configMap := es.configMapOperator.QueryConfigMapByName(service)
	if configMap == nil {
		return nil, "", errors.New("can't find configMap")
	}
	application := "application"
	if version != entity.DefaultProfile {
		application += "-" + version
	}
	application += ".yml"
	yamlString := configMap.Data[application]
	if yamlString != "" {
		err := yaml.Unmarshal([]byte(yamlString), &source)
		if err != nil {
			return nil, "", err
		}
	}

	return utils.ConvertRecursiveMapToSingleMap(source), configMap.Annotations[entity.ChoerodonVersion], nil
}

func isGateway(service string) bool {
	for _, v := range embed.Env.ConfigServer.GatewayNames {
		if v == service {
			return true
		}
	}
	return false
}

func addProperty(oldYaml string, newMap map[string]interface{}) (string, error) {
	oldMap := make(map[string]interface{})
	err := yaml.Unmarshal([]byte(oldYaml), &oldMap)
	if err != nil {
		return "", nil
	}
	recursiveAdd(oldMap, newMap)
	data, err := yaml.Marshal(oldMap)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func recursiveAdd(oldMap map[string]interface{}, newMap map[string]interface{}) {
	for nk, nv := range newMap {
		ov := oldMap[nk]
		if !utils.Contain(oldMap, nk) {
			oldMap[nk] = nv
		} else if nv != nil && reflect.TypeOf(nv).Kind() == reflect.Map && ov != nil && reflect.TypeOf(ov).Kind() == reflect.Map {
			recursiveAdd(ov.(map[string]interface{}), nv.(map[string]interface{}))
		}
	}
}

func separateRoute(gateway map[string]interface{}) (string, string, map[string]interface{}, error) {
	routeMap := make(map[string]interface{})
	for k, v := range gateway {
		if k == "zuul" && reflect.TypeOf(v).Kind() == reflect.Map {
			vm := v.(map[string]interface{})
			for rk, rv := range vm {
				if rk == "routes" {
					routeMap[rk] = rv
					delete(vm, rk)
				}
			}
		}
	}
	gb, err := yaml.Marshal(gateway)
	if err != nil {
		return "", "", nil, err
	}
	rm := map[string]interface{}{"zuul": routeMap}
	rb, err := yaml.Marshal(rm)
	if err != nil {
		return "", "", nil, err
	}
	return string(gb), string(rb), rm, nil
}
