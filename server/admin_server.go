package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/baetyl/baetyl-go/v2/cache"
	"github.com/baetyl/baetyl-go/v2/cache/persist"
	"github.com/baetyl/baetyl-go/v2/errors"
	"github.com/baetyl/baetyl-go/v2/log"
	"github.com/gin-gonic/gin"

	"github.com/baetyl/baetyl-cloud/v2/api"
	"github.com/baetyl/baetyl-cloud/v2/common"
	"github.com/baetyl/baetyl-cloud/v2/config"
	"github.com/baetyl/baetyl-cloud/v2/plugin"
	"github.com/baetyl/baetyl-cloud/v2/service"
)

// AdminServer admin server
type AdminServer struct {
	Auth             service.AuthService
	License          service.LicenseService
	Quota            service.QuotaService
	ExternalHandlers []gin.HandlerFunc
	APICache         persist.CacheStore

	cfg    *config.CloudConfig
	router *gin.Engine
	server *http.Server
	api    *api.API
	log    *log.Logger
}

const (
	DefaultAPICacheDuration = time.Second * 2
)

var (
	NodeCollector plugin.QuotaCollector
)

// NewAdminServer create admin server
func NewAdminServer(config *config.CloudConfig) (*AdminServer, error) {
	auth, err := service.NewAuthService(config)
	if err != nil {
		return nil, err
	}

	ls, err := service.NewLicenseService(config)
	if err != nil {
		return nil, err
	}

	qs, err := service.NewQuotaService(config)
	if err != nil {
		return nil, err
	}

	router := gin.New()
	server := &http.Server{
		Addr:           config.AdminServer.Port,
		Handler:        router,
		ReadTimeout:    config.AdminServer.ReadTimeout,
		WriteTimeout:   config.AdminServer.WriteTimeout,
		MaxHeaderBytes: 1 << 20,
	}
	return &AdminServer{
		cfg:      config,
		router:   router,
		server:   server,
		Auth:     auth,
		License:  ls,
		Quota:    qs,
		APICache: persist.NewInMemoryStore(DefaultAPICacheDuration),
		log:      log.L().With(log.Any("server", "AdminServer")),
	}, nil
}

func (s *AdminServer) Run() {
	if err := s.server.ListenAndServe(); err != nil {
		log.L().Info("admin server stopped", log.Error(err))
	}
}

func (s *AdminServer) SetAPI(api *api.API) {
	s.api = api
}

// Close server
func (s *AdminServer) Close() {
	ctx, _ := context.WithTimeout(context.Background(), s.cfg.AdminServer.ShutdownTime)
	s.server.Shutdown(ctx)
}

// InitRoute init router
func (s *AdminServer) InitRoute() {
	s.router.NoRoute(NoRouteHandler)
	s.router.NoMethod(NoMethodHandler)
	s.router.GET("/health", Health)
	s.router.Use(RequestIDHandler)
	s.router.Use(LoggerHandler)

	NodeCollector = s.api.NodeNumberCollector

	v1 := s.GetV1RouterGroup()
	{
		configs := v1.Group("/configs")
		configs.GET("/:name", s.WrapperCache(s.api.GetConfig))
		configs.PUT("/:name", common.WrapperWithLock(s.api.Locker.Lock, s.api.Locker.Unlock), common.Wrapper(s.api.UpdateConfig))
		configs.DELETE("/:name", common.WrapperRaw(s.api.ValidateResourceForDeleting, true), common.Wrapper(s.api.DeleteConfig))
		configs.POST("", common.WrapperRaw(s.api.ValidateResourceForCreating, true), common.Wrapper(s.api.CreateConfig))
		configs.GET("", s.WrapperCache(s.api.ListConfig))
		configs.GET("/:name/apps", common.Wrapper(s.api.GetAppByConfig))
	}
	{
		registry := v1.Group("/registries")
		registry.GET("/:name", common.Wrapper(s.api.GetRegistry))
		registry.PUT("/:name", common.Wrapper(s.api.UpdateRegistry))
		registry.POST("/:name/refresh", common.Wrapper(s.api.RefreshRegistryPassword))
		registry.DELETE("/:name", common.WrapperRaw(s.api.ValidateResourceForDeleting, true), common.Wrapper(s.api.DeleteRegistry))
		registry.POST("", common.WrapperRaw(s.api.ValidateResourceForCreating, true), common.Wrapper(s.api.CreateRegistry))
		registry.GET("", s.WrapperCache(s.api.ListRegistry))
		registry.GET("/:name/apps", common.Wrapper(s.api.GetAppByRegistry))
	}
	{
		certificate := v1.Group("/certificates")
		certificate.GET("/:name", common.Wrapper(s.api.GetCertificate))
		certificate.PUT("/:name", common.WrapperWithLock(s.api.Locker.Lock, s.api.Locker.Unlock), common.Wrapper(s.api.UpdateCertificate))
		certificate.DELETE("/:name", common.WrapperRaw(s.api.ValidateResourceForDeleting, true), common.Wrapper(s.api.DeleteCertificate))
		certificate.POST("", common.WrapperRaw(s.api.ValidateResourceForCreating, true), common.Wrapper(s.api.CreateCertificate))
		certificate.GET("", s.WrapperCache(s.api.ListCertificate))
		certificate.GET("/:name/apps", common.Wrapper(s.api.GetAppByCertificate))
	}
	{
		secrets := v1.Group("/secrets")
		secrets.GET("/:name", common.Wrapper(s.api.GetSecret))
		secrets.PUT("/:name", common.Wrapper(s.api.UpdateSecret))
		secrets.DELETE("/:name", common.WrapperRaw(s.api.ValidateResourceForDeleting, true), common.Wrapper(s.api.DeleteSecret))
		secrets.POST("", common.WrapperRaw(s.api.ValidateResourceForCreating, true), common.Wrapper(s.api.CreateSecret))
		secrets.GET("", s.WrapperCache(s.api.ListSecret))
		secrets.GET("/:name/apps", common.Wrapper(s.api.GetAppBySecret))
	}
	{
		nodes := v1.Group("/nodes")
		nodes.GET("/:name", s.WrapperCache(s.api.GetNode))
		nodes.PUT("", common.Wrapper(s.api.GetNodes))
		nodes.GET("/:name/apps", s.WrapperCache(s.api.GetAppByNode))
		nodes.GET("/:name/functions", common.Wrapper(s.api.GetFunctionsByNode))
		nodes.GET("/:name/stats", s.WrapperCache(s.api.GetNodeStats))
		nodes.PUT("/:name", common.WrapperWithLock(s.api.Locker.Lock, s.api.Locker.Unlock), common.Wrapper(s.api.UpdateNode))
		nodes.DELETE("/:name", common.Wrapper(s.api.DeleteNode))
		nodes.POST("", common.WrapperWithLock(s.api.Locker.Lock, s.api.Locker.Unlock), s.NodeQuotaHandler, common.Wrapper(s.api.CreateNode))
		nodes.GET("", s.WrapperCache(s.api.ListNode))
		nodes.GET("/:name/deploys", s.WrapperCache(s.api.GetNodeDeployHistory))
		nodes.GET("/:name/init", s.WrapperCache(s.api.GenInitCmdFromNode))
		nodes.PUT("/:name/mode", common.Wrapper(s.api.UpdateNodeMode))
		nodes.PUT("/:name/properties", common.Wrapper(s.api.UpdateNodeProperties))
		nodes.GET("/:name/properties", s.WrapperCache(s.api.GetNodeProperties))
		nodes.PUT("/:name/core/configs", common.Wrapper(s.api.UpdateCoreApp))
		nodes.GET("/:name/core/configs", s.WrapperCache(s.api.GetCoreAppConfigs))
		nodes.GET("/:name/core/versions", s.WrapperCache(s.api.GetCoreAppVersions))
	}
	{
		apps := v1.Group("/apps")
		apps.GET("/:name", s.WrapperCache(s.api.GetApplication))
		apps.GET("/:name/configs", s.WrapperCache(s.api.GetSysAppConfigs))
		apps.GET("/:name/secrets", s.WrapperCache(s.api.GetSysAppSecrets))
		apps.GET("/:name/certificates", s.WrapperCache(s.api.GetSysAppCertificates))
		apps.GET("/:name/registries", s.WrapperCache(s.api.GetSysAppRegistries))
		apps.PUT("/:name", common.WrapperWithLock(s.api.Locker.Lock, s.api.Locker.Unlock), common.Wrapper(s.api.UpdateApplication))
		apps.DELETE("/:name", common.WrapperRaw(s.api.ValidateResourceForDeleting, true), common.Wrapper(s.api.DeleteApplication))
		apps.POST("", common.WrapperRaw(s.api.ValidateResourceForCreating, true), common.WrapperWithLock(s.api.Locker.Lock, s.api.Locker.Unlock), common.Wrapper(s.api.CreateApplication))
		apps.GET("", s.WrapperCache(s.api.ListApplication))
	}
	{
		namespace := v1.Group("/namespace")
		namespace.POST("", common.Wrapper(s.api.CreateNamespace))
		namespace.GET("", s.WrapperCache(s.api.GetNamespace))
		namespace.DELETE("", common.Wrapper(s.api.DeleteNamespace))
	}
	{
		function := v1.Group("/functions")
		function.GET("", common.Wrapper(s.api.ListFunctionSources))
		if len(s.cfg.Plugin.Functions) != 0 {
			function.GET("/:source/functions", common.Wrapper(s.api.ListFunctions))
			function.GET("/:source/functions/:name/versions", common.Wrapper(s.api.ListFunctionVersions))
			function.POST("/:source/functions/:name/versions/:version", common.Wrapper(s.api.ImportFunction))
		}
	}
	{
		// Deprecated
		objects := v1.Group("/objects")
		objects.GET("", common.Wrapper(s.api.ListObjectSources))
		if len(s.cfg.Plugin.Objects) != 0 {
			objects.GET("/:source/buckets", common.Wrapper(s.api.ListBuckets))
			objects.GET("/:source/buckets/:bucket/objects", common.Wrapper(s.api.ListBucketObjects))
		}
	}

	{
		properties := v1.Group("properties")
		properties.GET("/:name", common.Wrapper(s.api.GetProperty))

		// TODO: deprecated, to use property api
		sysconfig := v1.Group("sysconfig")
		sysconfig.GET("/baetyl_version/latest", common.Wrapper(func(c *common.Context) (interface{}, error) {
			res, err := s.api.Module.GetLatestModule("baetyl")
			if err != nil {
				return nil, err
			}
			return map[string]string{
				"type":  "baetyl_version",
				"key":   "latest",
				"value": res.Version,
			}, nil
		}))
		sysconfig.GET("/baetyl-function-runtime", common.Wrapper(func(c *common.Context) (interface{}, error) {
			runtimes, err := s.api.Func.ListRuntimes()
			if err != nil {
				return nil, errors.Trace(err)
			}
			var runtimesView []map[string]string
			for k, v := range runtimes {
				runtimesView = append(runtimesView, map[string]string{
					"type":  "baetyl-function-runtime",
					"key":   k,
					"value": v,
				})
			}
			// {"sysconfigs":[{"type":"baetyl-function-runtime","key":"nodejs10","value":"hub.baidubce.com/baetyl/function-node:10.19-v2.0.0","createTime":"2020-08-20T05:16:27Z","updateTime":"2020-08-20T05:16:27Z"},{"type":"baetyl-function-runtime","key":"python3","value":"hub.baidubce.com/baetyl/function-python:3.6-v2.0.0","createTime":"2020-08-20T05:16:27Z","updateTime":"2020-08-20T05:16:27Z"},{"type":"baetyl-function-runtime","key":"python3-opencv","value":"hub.baidubce.com/baetyl/function-python-opencv:3.6","createTime":"2020-04-26T06:39:32Z","updateTime":"2020-04-26T06:39:32Z"},{"type":"baetyl-function-runtime","key":"sql","value":"hub.baidubce.com/baetyl-sandbox/function-sql:git-4a62dfc","createTime":"2020-08-20T05:16:27Z","updateTime":"2020-08-25T03:16:39Z"}]}
			return map[string]interface{}{
				"sysconfigs": runtimesView,
			}, nil
		}))
	}
	{
		module := v1.Group("modules")
		module.GET("", s.WrapperCache(s.api.ListModules))
		module.GET("/:name", s.WrapperCache(s.api.GetModules))
		module.GET("/:name/version/:version", s.WrapperCache(s.api.GetModuleByVersion))
		module.GET("/:name/latest", s.WrapperCache(s.api.GetLatestModule))
		module.POST("", common.Wrapper(s.api.CreateModule))
		module.PUT("/:name/version/:version", common.Wrapper(s.api.UpdateModule))
		module.DELETE("/:name", common.Wrapper(s.api.DeleteModules))
		module.DELETE("/:name/version/:version", common.Wrapper(s.api.DeleteModules))
	}
	{
		quotas := v1.Group("/quotas")
		quotas.GET("", s.WrapperCache(s.api.GetQuota))
	}
	{
		yaml := v1.Group("yaml")
		yaml.POST("", common.Wrapper(s.api.CreateYamlResource))
		yaml.PUT("", common.Wrapper(s.api.UpdateYamlResource))
		yaml.POST("/delete", common.Wrapper(s.api.DeleteYamlResource))
	}

	v2 := s.GetV2RouterGroup()
	{
		objects := v2.Group("/objects")
		objects.GET("", s.WrapperCache(s.api.ListObjectSourcesV2))
		if len(s.cfg.Plugin.Objects) != 0 {
			objects.GET("/:source/buckets", common.Wrapper(s.api.ListBucketsV2))
			objects.GET("/:source/buckets/:bucket/objects", common.Wrapper(s.api.ListBucketObjectsV2))
			objects.GET("/:source/buckets/:bucket/object", common.Wrapper(s.api.GetObjectPathV2))
			objects.GET("/:source/buckets/:bucket/object/put", common.Wrapper(s.api.GetObjectPutPathV2))
		}
	}
}

// GetRoute get router
func (s *AdminServer) GetRoute() *gin.Engine {
	return s.router
}

func (s *AdminServer) GetV1RouterGroup() *gin.RouterGroup {
	router := s.router.Group("v1")
	router.Use(s.AuthHandler)
	router.Use(s.ExternalHandlers...)
	return router
}

func (s *AdminServer) GetV2RouterGroup() *gin.RouterGroup {
	router := s.router.Group("v2")
	router.Use(s.AuthHandler)
	router.Use(s.ExternalHandlers...)
	return router
}

// auth handler
func (s *AdminServer) AuthHandler(c *gin.Context) {
	cc := common.NewContext(c)
	err := s.Auth.Authenticate(cc)
	if err != nil {
		s.log.Error("request authenticate failed",
			log.Any(cc.GetTrace()),
			log.Any("namespace", cc.GetNamespace()),
			log.Any("authorization", c.Request.Header.Get("Authorization")),
			log.Error(err))
		common.PopulateFailedResponse(cc, common.Error(common.ErrRequestAccessDenied, common.Field("error", err)), true)
	}
}

func (s *AdminServer) NodeQuotaHandler(c *gin.Context) {
	cc := common.NewContext(c)
	namespace := cc.GetNamespace()
	if err := s.api.Quota.CheckQuota(namespace, NodeCollector); err != nil {
		s.log.Error("quota out of limit",
			log.Any(cc.GetTrace()),
			log.Any("namespace", cc.GetNamespace()),
			log.Error(err))
		common.PopulateFailedResponse(cc, err, true)
	}
}

func (s *AdminServer) WrapperCache(handler common.HandlerFunc) func(c *gin.Context) {
	if s.cfg.AdminServer.CacheEnable {
		dur := DefaultAPICacheDuration
		if s.cfg.AdminServer.CacheDuration > 0 {
			dur = s.cfg.AdminServer.CacheDuration
		}
		return s.WrapperCacheDuration(handler, dur)
	}
	return common.Wrapper(handler)
}

func (s *AdminServer) WrapperCacheDuration(handler common.HandlerFunc, dur time.Duration) func(c *gin.Context) {
	return cache.WCacheByRequestURI(
		s.APICache,
		dur,
		common.Wrapper(handler),
		cache.WithLogger(s),
		cache.KeyWithGinContext([]string{"namespace"}),
		cache.WithoutHeader(),
		cache.WithoutHeaderIgnore([]string{"Content-Type"}),
	)
}

func (s *AdminServer) Errorf(msg string, vals ...interface{}) {
	s.log.Error(fmt.Sprintf(msg, vals...))
}
