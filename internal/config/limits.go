package config

import "github.com/KinasTomes/miles/internal/storage"

// DSGPolicyLimits trả bộ caps DSG P1 đã được Lead chốt, không phải AWS limits.
// Mỗi lần gọi sở hữu RestoreTiers riêng; không có global slice có thể bị mutate.
// Consumer dùng NewPolicyValidator(DSGPolicyLimits()); tests vẫn có thể inject
// PolicyLimits riêng. Không thay đổi defaults hoặc cấu hình runtime.
func DSGPolicyLimits() PolicyLimits {
	return PolicyLimits{
		MaxJSONBytes: 8192, MaxDecimalDigits: 128, MaxDecimalExponent: 256,
		MaxColdAfterDays: 1000, MaxAutoRestoreDays: 30, MaxReadaheadChunks: 64,
		RestoreTiers: []RestoreTierRule{
			{Class: storage.GlacierIR, Tier: "Standard"},
			{Class: storage.Glacier, Tier: "Expedited"},
			{Class: storage.Glacier, Tier: "Standard"},
			{Class: storage.Glacier, Tier: "Bulk"},
		},
	}
}

func supportedRestoreTier(rule RestoreTierRule) bool {
	return rule.Class == storage.GlacierIR && rule.Tier == "Standard" ||
		rule.Class == storage.Glacier && (rule.Tier == "Expedited" || rule.Tier == "Standard" || rule.Tier == "Bulk")
}
