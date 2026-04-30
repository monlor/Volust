package policy

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/robfig/cron/v3"
)

type Schedule struct {
	Expr string
}

type Retention struct {
	values []retentionValue
}

type retentionValue struct {
	key   string
	value string
}

var retentionKeys = map[string]string{
	"keep-last":    "--keep-last",
	"keep-daily":   "--keep-daily",
	"keep-weekly":  "--keep-weekly",
	"keep-monthly": "--keep-monthly",
	"keep-yearly":  "--keep-yearly",
	"keep-tag":     "--keep-tag",
}

func ParseSchedule(expr string) (Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("schedule must be a five-field cron expression")
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	if _, err := parser.Parse(expr); err != nil {
		return Schedule{}, err
	}
	return Schedule{Expr: expr}, nil
}

func ParseRetention(input string) (Retention, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return Retention{}, nil
	}

	var retention Retention
	for _, part := range strings.Split(input, ",") {
		key, rawValue, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return Retention{}, fmt.Errorf("retention %q must use key=value", part)
		}
		if _, ok := retentionKeys[key]; !ok {
			return Retention{}, fmt.Errorf("unsupported retention key %q", key)
		}
		rawValue = strings.TrimSpace(rawValue)
		if rawValue == "" {
			return Retention{}, fmt.Errorf("retention key %q must have a value", key)
		}
		if key != "keep-tag" {
			value, err := strconv.Atoi(rawValue)
			if err != nil || value < 1 {
				return Retention{}, fmt.Errorf("retention key %q must have a positive integer value", key)
			}
		}
		retention.values = append(retention.values, retentionValue{key: key, value: rawValue})
	}
	return retention, nil
}

func (r Retention) Args() []string {
	args := make([]string, 0, len(r.values)*2)
	for _, item := range r.values {
		args = append(args, retentionKeys[item.key], item.value)
	}
	return args
}
