package custom

import (
	"context"
	"fmt"
	"os"
	"strings"

	"pentagi/pkg/config"
	"pentagi/pkg/providers/pconfig"
	"pentagi/pkg/providers/provider"
	"pentagi/pkg/system"
	"pentagi/pkg/templates"

	"github.com/sirupsen/logrus"
	"github.com/vxcontrol/langchaingo/llms"
	"github.com/vxcontrol/langchaingo/llms/openai"
	"github.com/vxcontrol/langchaingo/llms/streaming"
)

// BaseDefaultOptions returns the fallback call options layered under every
// custom provider agent config. It is shared by config parsing so named
// channels are built with exactly the same baseline as the default channel.
func BaseDefaultOptions() []llms.CallOption {
	return []llms.CallOption{
		llms.WithTemperature(1.0),
		llms.WithTopP(1.0),
		llms.WithN(1),
		llms.WithMaxTokens(16384),
	}
}

func BuildProviderConfig(cfg *config.Config, configData []byte) (*pconfig.ProviderConfig, error) {
	providerConfig, err := pconfig.LoadConfigData(configData, BaseDefaultOptions())
	if err != nil {
		return nil, err
	}

	// A config that carries its own channel-wide model keeps it; the env model
	// must not leak into other channels (named providers seeded from this file
	// are re-parsed from the stored JSON here at runtime).
	if providerConfig.Model == "" && cfg.LLMServerModel != "" {
		providerConfig.AppendDefaultOptions(llms.WithModel(cfg.LLMServerModel))
	}

	return providerConfig, nil
}

func DefaultProviderConfig(cfg *config.Config) (*pconfig.ProviderConfig, error) {
	if cfg.LLMServerConfig == "" {
		return BuildProviderConfig(cfg, []byte(pconfig.EmptyProviderConfigRaw))
	}

	configData, err := os.ReadFile(cfg.LLMServerConfig)
	if err != nil {
		return nil, err
	}

	return BuildProviderConfig(cfg, configData)
}

type customProvider struct {
	llm            *openai.LLM
	model          string
	models         pconfig.ModelsConfig
	providerName   provider.ProviderName
	providerConfig *pconfig.ProviderConfig
	providerPrefix string
}

type customAPI struct {
	apiType openai.APIType
	version string
}

func parseCustomAPI(apiType, apiVersion string) (customAPI, error) {
	declared := strings.ToLower(strings.TrimSpace(apiType))
	if declared == "" {
		return customAPI{}, nil
	}

	known, ok := map[string]openai.APIType{
		"openai":   openai.APITypeOpenAI,
		"azure":    openai.APITypeAzure,
		"azure_ad": openai.APITypeAzureAD,
	}[declared]
	if !ok {
		return customAPI{}, fmt.Errorf(
			"unknown api_type %q for provider %q: expected openai, azure or azure_ad",
			apiType, provider.ProviderCustom,
		)
	}

	return customAPI{apiType: known, version: strings.TrimSpace(apiVersion)}, nil
}

func (a customAPI) clientOptions() []openai.Option {
	if a.apiType == "" {
		return nil
	}

	opts := []openai.Option{openai.WithAPIType(a.apiType)}
	if a.version != "" {
		opts = append(opts, openai.WithAPIVersion(a.version))
	}

	return opts
}

func (a customAPI) isAzure() bool {
	return a.apiType == openai.APITypeAzure || a.apiType == openai.APITypeAzureAD
}

func New(
	cfg *config.Config,
	providerName provider.ProviderName,
	providerConfig *pconfig.ProviderConfig,
	enrich func(pconfig.ModelsConfig) pconfig.ModelsConfig,
) (provider.Provider, error) {
	// Per-channel config values win; the LLM_SERVER_* environment variables are
	// only the bootstrap fallback so existing deployments behave as before.
	var cfgView pconfig.ProviderConfig
	if providerConfig != nil {
		cfgView = *providerConfig
	}
	baseKey := pconfig.FirstNonEmpty(cfgView.Key, cfg.LLMServerKey)
	baseURL := pconfig.FirstNonEmpty(cfgView.URL, cfg.LLMServerURL)
	baseModel := pconfig.FirstNonEmpty(cfgView.Model, cfg.LLMServerModel)
	if baseKey == "" {
		return nil, fmt.Errorf(
			"missing API key for provider %q: set the key field in its config or the LLM_SERVER_KEY environment variable",
			provider.ProviderCustom,
		)
	}

	httpClient, err := system.GetHTTPClient(cfg)
	if err != nil {
		return nil, err
	}

	opts := []openai.Option{
		openai.WithToken(baseKey),
		openai.WithModel(baseModel),
		openai.WithBaseURL(baseURL),
		openai.WithHTTPClient(httpClient),
	}
	if cfg.LLMServerPreserveReasoning {
		opts = append(opts,
			openai.WithPreserveReasoningContent(),
		)
	}

	api, err := parseCustomAPI(
		pconfig.FirstNonEmpty(cfgView.APIType, cfg.LLMServerAPIType),
		pconfig.FirstNonEmpty(cfgView.APIVersion, cfg.LLMServerAPIVersion),
	)
	if err != nil {
		return nil, err
	}
	opts = append(opts, api.clientOptions()...)
	client, err := openai.New(opts...)
	if err != nil {
		return nil, err
	}

	models := pconfig.ModelsConfig{}
	if !api.isAzure() {
		loaded, err := provider.LoadModelsFromHTTP(baseURL, baseKey, httpClient, cfg.LLMServerProvider)
		if err != nil {
			logrus.WithError(err).WithFields(logrus.Fields{
				"provider": providerName,
				"url":      baseURL,
			}).Warn("custom provider could not list its models; model limits and capabilities stay unknown")
		} else {
			models = loaded
		}
	}
	if enrich != nil {
		models = enrich(models)
	}

	return &customProvider{
		llm:            client,
		model:          baseModel,
		models:         models,
		providerName:   providerName,
		providerConfig: providerConfig,
		providerPrefix: cfg.LLMServerProvider,
	}, nil
}

func (p *customProvider) Type() provider.ProviderType {
	return provider.ProviderCustom
}

func (p *customProvider) Name() provider.ProviderName {
	return p.providerName
}

func (p *customProvider) GetRawConfig() []byte {
	return p.providerConfig.GetRawConfig()
}

func (p *customProvider) GetProviderConfig() *pconfig.ProviderConfig {
	return p.providerConfig
}

func (p *customProvider) GetPriceInfo(opt pconfig.ProviderOptionsType) *pconfig.PriceInfo {
	return p.providerConfig.GetPriceInfoForType(opt)
}

func (p *customProvider) GetModels() pconfig.ModelsConfig {
	return p.models
}

func (p *customProvider) Model(opt pconfig.ProviderOptionsType) string {
	model := p.model
	opts := llms.CallOptions{Model: &model}
	for _, option := range p.providerConfig.GetOptionsForType(opt) {
		option(&opts)
	}

	return opts.GetModel()
}

func (p *customProvider) ModelWithPrefix(opt pconfig.ProviderOptionsType) string {
	return provider.ApplyModelPrefix(p.Model(opt), p.providerPrefix)
}

func (p *customProvider) Call(
	ctx context.Context,
	opt pconfig.ProviderOptionsType,
	prompt string,
) (string, error) {
	return provider.WrapGenerateFromSinglePrompt(
		ctx, p, opt, p.llm, prompt,
		p.providerConfig.GetOptionsForType(opt)...,
	)
}

func (p *customProvider) CallEx(
	ctx context.Context,
	opt pconfig.ProviderOptionsType,
	chain []llms.MessageContent,
	streamCb streaming.Callback,
) (*llms.ContentResponse, error) {
	return provider.WrapGenerateContent(
		ctx, p, opt, p.llm.GenerateContent, chain,
		append([]llms.CallOption{
			llms.WithStreamingFunc(streamCb),
		}, p.providerConfig.GetOptionsForType(opt)...)...,
	)
}

func (p *customProvider) CallWithTools(
	ctx context.Context,
	opt pconfig.ProviderOptionsType,
	chain []llms.MessageContent,
	tools []llms.Tool,
	streamCb streaming.Callback,
) (*llms.ContentResponse, error) {
	return provider.WrapGenerateContent(
		ctx, p, opt, p.llm.GenerateContent, chain,
		append([]llms.CallOption{
			llms.WithTools(tools),
			llms.WithStreamingFunc(streamCb),
		}, p.providerConfig.GetOptionsForType(opt)...)...,
	)
}

func (p *customProvider) CallWithExtraOptions(
	ctx context.Context,
	opt pconfig.ProviderOptionsType,
	chain []llms.MessageContent,
	tools []llms.Tool,
	streamCb streaming.Callback,
	extra ...llms.CallOption,
) (*llms.ContentResponse, error) {
	options := []llms.CallOption{llms.WithStreamingFunc(streamCb)}
	if len(tools) > 0 {
		options = append(options, llms.WithTools(tools))
	}
	options = append(options, p.providerConfig.GetOptionsForType(opt)...)
	options = append(options, extra...)

	return provider.WrapGenerateContent(ctx, p, opt, p.llm.GenerateContent, chain, options...)
}

func (p *customProvider) GetUsage(info map[string]any) pconfig.CallUsage {
	return pconfig.NewCallUsage(info)
}

func (p *customProvider) GetToolCallIDTemplate(ctx context.Context, prompter templates.Prompter) (string, error) {
	return provider.DetermineToolCallIDTemplate(ctx, p, pconfig.OptionsTypeSimple, prompter, "")
}
