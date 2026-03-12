package filtering

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/AdguardTeam/AdGuardHome/internal/aghhttp"
	"github.com/AdguardTeam/AdGuardHome/internal/filtering/rulelist"
	"github.com/AdguardTeam/AdGuardHome/internal/schedule"
	"github.com/AdguardTeam/golibs/log"
	"github.com/AdguardTeam/urlfilter/rules"
)

// serviceRules maps a service ID to its filtering rules.
var serviceRules map[string][]*rules.NetworkRule

// serviceIDs contains service IDs sorted alphabetically.
var serviceIDs []string

// serviceLoader is the global service loader instance.
var serviceLoader *ServiceLoader

// serviceLoaderMu protects the service loader instance.
var serviceLoaderMu sync.RWMutex

// serviceRulesMu protects serviceIDs and serviceRules.
var serviceRulesMu sync.RWMutex

// initBlockedServices initializes package-level blocked service data.
func initBlockedServices() {
	serviceRulesMu.Lock()
	defer serviceRulesMu.Unlock()
	l := len(blockedServices)
	serviceIDs = make([]string, l)
	serviceRules = make(map[string][]*rules.NetworkRule, l)
	for i, s := range blockedServices {
		netRules := make([]*rules.NetworkRule, 0, len(s.Rules))
		for _, text := range s.Rules {
			rule, err := rules.NewNetworkRule(text, rulelist.URLFilterIDBlockedService)
			if err != nil {
				log.Error("parsing blocked service %q rule %q: %s", s.ID, text, err)
				continue
			}

			netRules = append(netRules, rule)
		}
		serviceIDs[i] = s.ID
		serviceRules[s.ID] = netRules
	}
	slices.Sort(serviceIDs)
	log.Debug("filtering: initialized %d services", l)
}

// InitServiceLoader initializes the service loader with the configured URLs.
// It is called when the DNSFilter is created.
func (d *DNSFilter) initServiceLoader() {
	if len(d.conf.ServiceURLs) == 0 {
		// d.conf.ServiceURLs = []string{"https://www.nullprivate.com/services/i18n/zh-cn.json"}
		// d.conf.ServiceURLs = []string{"https://hostlistsregistry.nullprivate.com/assets/services.en-us.json"}
		d.conf.ServiceURLs = []string{"https://hostlistsregistry.adguardprivate.com/assets/services.zh-cn.json"}
	}

	logger := slog.Default()
	if d.logger != nil {
		logger = d.logger
	}

	newLoader := NewServiceLoader(
		d.conf.ServiceURLs,
		d.conf.DataDir,
		d.conf.HTTPClient,
		logger,
	)

	// Use the service loader mutex to ensure that only one instance is created
	// at a time.
	serviceLoaderMu.Lock()
	serviceLoader = newLoader
	serviceLoaderMu.Unlock()

	// 预加载服务：使用与请求无关的后台上下文，避免 r.Context() 很快被取消导致“context canceled”
	go func() {
		// 在goroutine中使用读锁访问 serviceLoader
		serviceLoaderMu.RLock()
		loader := serviceLoader
		serviceLoaderMu.RUnlock()

		// 预加载使用独立的超时上下文
		preloadCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if loader != nil {
			_, err := loader.LoadServices(preloadCtx)
			if err != nil {
				log.Error("filtering: failed to load services: %s", err)
			}
		}
	}()
}

// updateBlockedServicesFromLoader 从加载器中更新服务规则
func updateBlockedServicesFromLoader(ctx context.Context) {
	serviceLoaderMu.RLock()
	loader := serviceLoader
	serviceLoaderMu.RUnlock()

	if loader == nil {
		return
	}
	services := loader.GetBlockedServices(ctx)
	if len(services) == 0 {
		log.Debug("filtering: no services loaded from URLs")
		return
	}

	// 更新服务规则
	newServiceIDs := make([]string, len(services))
	newServiceRules := make(map[string][]*rules.NetworkRule, len(services))

	for i, s := range services {
		netRules := make([]*rules.NetworkRule, 0, len(s.Rules))
		for _, text := range s.Rules {
			rule, err := rules.NewNetworkRule(text, rulelist.URLFilterIDBlockedService)
			if err != nil {
				log.Error("parsing blocked service %q rule %q: %s", s.ID, text, err)
				continue
			}
			netRules = append(netRules, rule)
		}
		newServiceIDs[i] = s.ID
		newServiceRules[s.ID] = netRules
	}

	slices.Sort(newServiceIDs)

	// 使用读写锁保护全局变量
	serviceRulesMu.Lock()
	serviceIDs = newServiceIDs
	serviceRules = newServiceRules
	serviceRulesMu.Unlock()

	log.Debug("filtering: updated %d services from dynamic sources", len(services))
}

// filterKnownServiceIDs 过滤出当前已知（可用）的服务 ID。
// 返回值：第一个为保留的已知 ID，第二个为被丢弃的未知 ID。
func filterKnownServiceIDs(list []string) (kept, dropped []string) {
	if len(list) == 0 {
		return nil, nil
	}
	serviceRulesMu.RLock()
	defer serviceRulesMu.RUnlock()
	kept = make([]string, 0, len(list))
	dropped = make([]string, 0)
	for _, id := range list {
		if _, ok := serviceRules[id]; ok {
			kept = append(kept, id)
		} else {
			dropped = append(dropped, id)
		}
	}
	return kept, dropped
}

// SanitizeBlockedServiceIDs 导出方法：用于在启动或加载配置时，
// 将未知的服务 ID 从列表中剔除，避免因校验失败阻止系统启动。
// 注意：该函数不会返回错误，仅返回保留与丢弃的列表。
func SanitizeBlockedServiceIDs(list []string) (kept, dropped []string) {
	return filterKnownServiceIDs(list)
}

// BlockedServices is the configuration of blocked services.
type BlockedServices struct {
	// Schedule is blocked services schedule for every day of the week.
	Schedule *schedule.Weekly `json:"schedule" yaml:"schedule"`
	// IDs is the names of blocked services.
	IDs []string `json:"ids" yaml:"ids"`
}

// Clone returns a deep copy of blocked services.
func (s *BlockedServices) Clone() (c *BlockedServices) {
	if s == nil {
		return nil
	}
	return &BlockedServices{
		Schedule: s.Schedule.Clone(),
		IDs:      slices.Clone(s.IDs),
	}
}

// Validate returns an error if blocked services contain unknown service ID.  s
// must not be nil.
func (s *BlockedServices) Validate() (err error) {
	serviceRulesMu.RLock()
	defer serviceRulesMu.RUnlock()
	for _, id := range s.IDs {
		_, ok := serviceRules[id]
		if !ok {
			return fmt.Errorf("unknown blocked-service %q", id)
		}
	}
	return nil
}

// ApplyBlockedServices - set blocked services settings for this DNS request
func (d *DNSFilter) ApplyBlockedServices(setts *Settings) {
	d.confMu.RLock()
	defer d.confMu.RUnlock()

	setts.ServicesRules = []ServiceEntry{}

	bsvc := d.conf.BlockedServices

	// TODO(s.chzhen):  Use startTime from [dnsforward.dnsContext].
	if !bsvc.Schedule.Contains(time.Now()) {
		d.ApplyBlockedServicesList(setts, bsvc.IDs)
	}
}

// ApplyBlockedServicesList appends filtering rules to the settings.
func (d *DNSFilter) ApplyBlockedServicesList(setts *Settings, list []string) {
	serviceRulesMu.RLock()
	defer serviceRulesMu.RUnlock()
	for _, name := range list {
		rules, ok := serviceRules[name]
		if !ok {
			log.Error("unknown service name: %s", name)

			continue
		}
		setts.ServicesRules = append(setts.ServicesRules, ServiceEntry{
			Name:  name,
			Rules: rules,
		})
	}
}

func (d *DNSFilter) handleBlockedServicesIDs(w http.ResponseWriter, r *http.Request) {
	// 在获取规则列表前动态更新服务规则
	updateBlockedServicesFromLoader(r.Context())

	serviceRulesMu.RLock()
	ids := slices.Clone(serviceIDs)
	serviceRulesMu.RUnlock()
	aghhttp.WriteJSONResponseOK(w, r, ids)
}

func (d *DNSFilter) handleBlockedServicesAll(w http.ResponseWriter, r *http.Request) {
	// 在获取规则列表前动态更新服务规则
	updateBlockedServicesFromLoader(r.Context())

	allServices := blockedServices
	if serviceLoader != nil {
		allServices = serviceLoader.GetBlockedServices(r.Context())
	}

	aghhttp.WriteJSONResponseOK(w, r, struct {
		BlockedServices []blockedService `json:"blocked_services"`
	}{
		BlockedServices: allServices,
	})
}

// handleBlockedServicesList is the handler for the GET
// /control/blocked_services/list HTTP API.
//
// Deprecated:  Use handleBlockedServicesGet.
func (d *DNSFilter) handleBlockedServicesList(w http.ResponseWriter, r *http.Request) {
	var list []string
	func() {
		d.confMu.Lock()
		defer d.confMu.Unlock()
		if d.conf.BlockedServices != nil {
			list = d.conf.BlockedServices.IDs
		} else {
			list = []string{}
		}
	}()
	aghhttp.WriteJSONResponseOK(w, r, list)
}

// handleBlockedServicesSet is the handler for the POST
// /control/blocked_services/set HTTP API.
//
// Deprecated:  Use handleBlockedServicesUpdate.
func (d *DNSFilter) handleBlockedServicesSet(w http.ResponseWriter, r *http.Request) {
	list := []string{}
	err := json.NewDecoder(r.Body).Decode(&list)
	if err != nil {
		aghhttp.Error(r, w, http.StatusBadRequest, "json.Decode: %s", err)
		return
	}

	// 在设置规则前动态更新服务规则
	updateBlockedServicesFromLoader(r.Context())

	// 规范化为 id 并丢弃未知项
	ids, dropped := SanitizeBlockedServiceIDs(list)
	if len(dropped) > 0 {
		log.Debug("blocked_services.set: dropping unknown ids: %v", dropped)
	}

	func() {
		d.confMu.Lock()
		defer d.confMu.Unlock()
		if d.conf.BlockedServices == nil {
			d.conf.BlockedServices = &BlockedServices{
				Schedule: schedule.EmptyWeekly(),
				IDs:      ids,
			}
		} else {
			d.conf.BlockedServices.IDs = ids
		}
		log.Debug("Updated blocked services list: %d", len(ids))
	}()
	d.conf.ConfigModified()
}

// handleBlockedServicesGet is the handler for the GET
// /control/blocked_services/get HTTP API.
func (d *DNSFilter) handleBlockedServicesGet(w http.ResponseWriter, r *http.Request) {
	var bsvc *BlockedServices
	func() {
		d.confMu.RLock()
		defer d.confMu.RUnlock()
		if d.conf.BlockedServices != nil {
			bsvc = d.conf.BlockedServices.Clone()
		} else {
			bsvc = &BlockedServices{
				Schedule: schedule.EmptyWeekly(),
				IDs:      []string{},
			}
		}
	}()
	// 保证 JSON 返回中 `ids` 不为 null
	if bsvc != nil && bsvc.IDs == nil {
		bsvc.IDs = []string{}
	}
	aghhttp.WriteJSONResponseOK(w, r, bsvc)
}

// handleBlockedServicesUpdate is the handler for the PUT
// /control/blocked_services/update HTTP API.
func (d *DNSFilter) handleBlockedServicesUpdate(w http.ResponseWriter, r *http.Request) {
	bsvc := &BlockedServices{}
	err := json.NewDecoder(r.Body).Decode(bsvc)
	if err != nil {
		aghhttp.Error(r, w, http.StatusBadRequest, "json.Decode: %s", err)
		return
	}

	// 在更新规则前动态更新服务规则
	updateBlockedServicesFromLoader(r.Context())

	// 规范化并过滤请求中的服务：仅按 ID 丢弃未知，避免 422。
	kept, dropped := SanitizeBlockedServiceIDs(bsvc.IDs)
	if len(dropped) > 0 {
		log.Debug("blocked_services.update: dropping unknown ids: %v", dropped)
	}
	bsvc.IDs = kept

	err = bsvc.Validate()
	if err != nil {
		aghhttp.Error(r, w, http.StatusUnprocessableEntity, "validating: %s", err)
		return
	}
	if bsvc.Schedule == nil {
		bsvc.Schedule = schedule.EmptyWeekly()
	}
	func() {
		d.confMu.Lock()
		defer d.confMu.Unlock()
		d.conf.BlockedServices = bsvc
	}()
	log.Debug("updated blocked services schedule: %d", len(bsvc.IDs))
	d.conf.ConfigModified()
}

// handleBlockedServicesReload is the handler for the POST
// /control/blocked_services/reload HTTP API
func (d *DNSFilter) handleBlockedServicesReload(w http.ResponseWriter, r *http.Request) {
	// 确保已初始化 loader（避免因进程生命周期差异导致的空指针场景）
	serviceLoaderMu.RLock()
	loader := serviceLoader
	serviceLoaderMu.RUnlock()
	if loader == nil {
		d.initServiceLoader()
		serviceLoaderMu.RLock()
		loader = serviceLoader
		serviceLoaderMu.RUnlock()
	}

	// 读取当前配置的 service_urls
	var urls []string
	func() {
		d.confMu.RLock()
		defer d.confMu.RUnlock()
		if d.conf.ServiceURLs != nil {
			urls = slices.Clone(d.conf.ServiceURLs)
		}
	}()

	// 若未配置服务配置源：不视为错误，回退并保留/刷新内置服务
	if len(urls) == 0 {
		initBlockedServices()
		serviceRulesMu.RLock()
		count := len(serviceIDs)
		serviceRulesMu.RUnlock()
		aghhttp.WriteJSONResponseOK(w, r, struct {
			Status  string `json:"status"`
			Count   int    `json:"count"`
			Message string `json:"message"`
		}{
			Status:  "ok",
			Count:   count,
			Message: "未配置服务源，已使用内置服务",
		})
		return
	}

	// 使用后台超时上下文，避免请求被客户端中断导致 context canceled
	reloadCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 尝试重新加载；失败时降级为使用内置/缓存数据并返回 200，避免 500
	_, err := loader.LoadServices(reloadCtx)
	if err != nil {
		log.Error("failed to reload services, falling back: %s", err)
	}

	updateBlockedServicesFromLoader(r.Context())

	msg := "服务已重新加载"
	if err != nil {
		msg = "服务源加载失败，已回退至内置/缓存数据"
	}

	aghhttp.WriteJSONResponseOK(w, r, struct {
		Status  string `json:"status"`
		Count   int    `json:"count"`
		Message string `json:"message"`
	}{
		Status:  "ok",
		Count:   func() int { serviceRulesMu.RLock(); defer serviceRulesMu.RUnlock(); return len(serviceIDs) }(),
		Message: msg,
	})
}

// handleServiceURLsGet 获取当前配置的 service_urls
func (d *DNSFilter) handleServiceURLsGet(w http.ResponseWriter, r *http.Request) {
	var urls []string
	func() {
		d.confMu.RLock()
		defer d.confMu.RUnlock()
		if d.conf.ServiceURLs != nil {
			urls = slices.Clone(d.conf.ServiceURLs)
		} else {
			urls = []string{}
		}
	}()
	aghhttp.WriteJSONResponseOK(w, r, struct {
		ServiceURLs []string `json:"service_urls"`
	}{
		ServiceURLs: urls,
	})
}

// handleServiceURLsSet 设置 service_urls
func (d *DNSFilter) handleServiceURLsSet(w http.ResponseWriter, r *http.Request) {
	var data struct {
		ServiceURLs []string `json:"service_urls"`
	}
	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		aghhttp.Error(r, w, http.StatusBadRequest, "json.Decode: %s", err)
		return
	}

	if len(data.ServiceURLs) == 0 {
		// Use default value
		data.ServiceURLs = []string{"https://hostlistsregistry.adguardprivate.com/assets/services.zh-cn.json"}
	}

	func() {
		d.confMu.Lock()
		defer d.confMu.Unlock()
		d.conf.ServiceURLs = data.ServiceURLs
	}()

	// Reinitialize service loader（不绑定请求生命周期）
	d.initServiceLoader()

	// Reload services：使用后台超时上下文，避免客户端断开导致取消
	if serviceLoader != nil {
		reloadCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		_, loadErr := serviceLoader.LoadServices(reloadCtx)
		if loadErr != nil {
			log.Error("failed to reload services: %s", loadErr)
		}
		updateBlockedServicesFromLoader(r.Context())
	}

	// 在更新 ServiceURLs 后，对当前全局 blocked_services 做一次规范化与清理：
	// 仅保留仍然有效的 ID，删除因源变化而变为未知的项，避免后续更新时报 422。
	func() {
		d.confMu.Lock()
		defer d.confMu.Unlock()
		if d.conf.BlockedServices != nil && len(d.conf.BlockedServices.IDs) > 0 {
			kept, dropped := SanitizeBlockedServiceIDs(d.conf.BlockedServices.IDs)
			if len(dropped) > 0 {
				log.Debug("service_urls.set: removed unknown blocked-service ids after source change: %v", dropped)
			}
			d.conf.BlockedServices.IDs = kept
		}
	}()

	log.Debug("Updated service URLs: %d", len(data.ServiceURLs))
	d.conf.ConfigModified()

	aghhttp.WriteJSONResponseOK(w, r, struct {
		Status  string   `json:"status"`
		URLs    []string `json:"urls"`
		Message string   `json:"message"`
	}{
		Status:  "ok",
		URLs:    data.ServiceURLs,
		Message: "Service URLs updated",
	})
}
