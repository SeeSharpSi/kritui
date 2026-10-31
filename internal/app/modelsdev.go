package app

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strings"

	"seesharpsi/kritui/llm"
	"seesharpsi/kritui/modelsdev"
	"seesharpsi/kritui/templ"
)

// modelsDevService is initialized once before the HTTP server starts. A nil
// service disables catalog enrichment without affecting provider discovery.
var modelsDevService *modelsdev.Service

// configureModelsDev loads the cached public catalog and refreshes it in the
// background. Failures never stop the app from serving requests.
func configureModelsDev(ctx context.Context, database *sql.DB) {
	modelsDevService = nil
	if strings.EqualFold(strings.TrimSpace(os.Getenv("MODELS_DEV")), "off") {
		log.Printf("models.dev disabled")
		return
	}
	service, err := modelsdev.NewService(modelsdev.NewSQLCache(database), modelsdev.Options{
		URL: os.Getenv("MODELS_DEV_URL"),
	})
	if err != nil {
		log.Printf("configure models.dev: %v", err)
		return
	}
	modelsDevService = service
	service.Start(ctx)
}

func catalogProviderModel(endpoint, model string) (modelsdev.ProviderModel, bool) {
	if modelsDevService == nil {
		return modelsdev.ProviderModel{}, false
	}
	return modelsDevService.Lookup(endpoint, model)
}

func catalogModelSupport(endpoint, model string) (llm.ModelInfo, bool) {
	providerModel, ok := catalogProviderModel(endpoint, model)
	if !ok {
		return llm.ModelInfo{}, false
	}
	info := llm.ModelInfo{ID: model}
	known := false
	if supported, available := providerModel.Model.SupportsImage(); available {
		info.ImageSupport = supportValue(supported)
		known = true
	}
	if supported, available := providerModel.Model.SupportsPDF(); available {
		info.PDFSupport = supportValue(supported)
		known = true
	}
	return info, known
}

func supportValue(supported bool) llm.InputSupport {
	if supported {
		return llm.InputSupportSupported
	}
	return llm.InputSupportUnsupported
}

// mergeModelSupport keeps explicit provider metadata authoritative and fills
// unknown fields from models.dev.
func mergeModelSupport(primary llm.ModelInfo, primaryKnown bool, fallback llm.ModelInfo, fallbackKnown bool) (llm.ModelInfo, bool) {
	if !primaryKnown {
		return fallback, fallbackKnown
	}
	if fallbackKnown {
		if primary.ImageSupport == llm.InputSupportUnknown {
			primary.ImageSupport = fallback.ImageSupport
		}
		if primary.PDFSupport == llm.InputSupportUnknown {
			primary.PDFSupport = fallback.PDFSupport
		}
	}
	return primary, true
}

// modelCostEstimator returns a models.dev estimator used only when the
// provider response itself omits cost.
func modelCostEstimator(endpoint, model string) func(llm.Usage) (float64, bool) {
	providerModel, ok := catalogProviderModel(endpoint, model)
	if !ok || providerModel.Model.Cost == nil {
		return nil
	}
	return func(usage llm.Usage) (float64, bool) {
		return providerModel.Model.EstimateCost(
			usage.InputTokens,
			usage.CachedInputTokens,
			usage.CacheWriteTokens,
			usage.OutputTokens,
		)
	}
}

func availableModelDetails(ctx context.Context, database *sql.DB, modelIDs []string) map[string]templates.ModelDetail {
	config, err := effectiveLLMConfig(ctx, database)
	if err != nil {
		log.Printf("get LLM config for model details: %v", err)
		return nil
	}
	return modelDetailsForEndpoint(config.Endpoint, modelIDs)
}

func modelDetailsForEndpoint(endpoint string, modelIDs []string) map[string]templates.ModelDetail {
	if modelsDevService == nil || len(modelIDs) == 0 {
		return nil
	}
	details := make(map[string]templates.ModelDetail)
	for _, modelID := range modelIDs {
		providerModel, ok := catalogProviderModel(endpoint, modelID)
		if !ok {
			continue
		}
		detail := modelDetail(providerModel)
		if len(detail.Modalities) == 0 {
			continue
		}
		details[modelID] = detail
	}
	if len(details) == 0 {
		return nil
	}
	return details
}

func modelDetail(providerModel modelsdev.ProviderModel) templates.ModelDetail {
	modalities := make([]string, 0, len(providerModel.Model.Modalities.Input))
	seen := make(map[string]struct{}, len(providerModel.Model.Modalities.Input))
	for _, modality := range providerModel.Model.Modalities.Input {
		kind := modalityKind(modality)
		if kind == "" {
			continue
		}
		if _, ok := seen[kind]; ok {
			continue
		}
		seen[kind] = struct{}{}
		modalities = append(modalities, kind)
	}
	return templates.ModelDetail{Modalities: modalities}
}

func modalityKind(modality string) string {
	modality = strings.ToLower(strings.TrimSpace(modality))
	switch {
	case modality == "text" || strings.HasPrefix(modality, "text/"):
		return "text"
	case modality == "image" || strings.HasPrefix(modality, "image/"):
		return "image"
	case modality == "pdf" || modality == "application/pdf" || modality == "file" || modality == "document":
		return "pdf"
	case modality == "audio" || strings.HasPrefix(modality, "audio/"):
		return "audio"
	case modality == "video" || strings.HasPrefix(modality, "video/"):
		return "video"
	default:
		return ""
	}
}
