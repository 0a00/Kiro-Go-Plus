package proxy

import (
	"regexp"
	"strings"

	"kiro-go/config"
)

var (
	kiroClaudeMinorModelPattern     = regexp.MustCompile(`^claude-(opus|sonnet|haiku)-(\d+)\.(\d{1,2})((?:-(?:\d{8}|latest))?(?:-thinking)?)$`)
	officialClaudeMinorModelPattern = regexp.MustCompile(`^claude-(opus|sonnet|haiku)-(\d+)-(\d{1,2})((?:-(?:\d{8}|latest))?(?:-thinking)?)$`)
)

// modelIDForAPI selects the external representation of Claude minor versions
// without changing non-Claude IDs or custom aliases.
func modelIDForAPI(model string, useOfficialNames bool) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return model
	}
	lower := strings.ToLower(model)
	if match := kiroClaudeMinorModelPattern.FindStringSubmatch(lower); match != nil {
		if !useOfficialNames {
			return model
		}
		return "claude-" + match[1] + "-" + match[2] + "-" + match[3] + match[4]
	}
	if match := officialClaudeMinorModelPattern.FindStringSubmatch(lower); match != nil {
		if useOfficialNames {
			return model
		}
		return "claude-" + match[1] + "-" + match[2] + "." + match[3] + match[4]
	}
	return model
}

func exposedModelID(model string) string {
	return modelIDForAPI(model, config.GetUseOfficialModelNames())
}
