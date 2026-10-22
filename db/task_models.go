package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var ErrTaskModelInvalid = errors.New("invalid task model selection")

func normalizedTaskModels(models map[int64]string) map[int64]string {
	var result map[int64]string
	for id, model := range models {
		if model = strings.TrimSpace(model); model != "" {
			if result == nil {
				result = make(map[int64]string)
			}
			result[id] = model
		}
	}
	return result
}

func (d *DB) ValidateTaskModels(ids []int64, models map[int64]string) error {
	return validateTaskModels(d, ids, models)
}

func validateTaskModels(q interface{ QueryRow(string, ...any) *sql.Row }, ids []int64, models map[int64]string) error {
	allowed := make(map[int64]bool, len(ids))
	for _, id := range ids {
		allowed[id] = true
	}
	for id, model := range models {
		if !allowed[id] {
			return fmt.Errorf("%w: Provider #%d 不在任务配置链中", ErrTaskModelInvalid, id)
		}
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		var current string
		var raw []byte
		if err := q.QueryRow(`SELECT model, models FROM llm_profiles WHERE id=$1`, id).Scan(&current, &raw); err != nil {
			return fmt.Errorf("%w: load Provider #%d: %v", ErrTaskModelInvalid, id, err)
		}
		var candidates []string
		if err := json.Unmarshal(raw, &candidates); err != nil {
			return err
		}
		found := model == current
		for _, candidate := range candidates {
			found = found || candidate == model
		}
		if !found {
			return fmt.Errorf("%w: 模型 %q 不在 Provider #%d 的模型列表中", ErrTaskModelInvalid, model, id)
		}
	}
	return nil
}
