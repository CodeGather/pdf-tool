package config

import (
	"encoding/json"
	"fmt"
	"os"

	"example.com/pdf-tool/model"
)

// LoadConfig 读取并解析 JSON 配置文件
func LoadConfig(path string) (*model.ReplaceConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取文件失败: %w", err)
	}

	var cfg model.ReplaceConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("数据校验失败: %w", err)
	}

	return &cfg, nil
}

func validate(cfg *model.ReplaceConfig) error {
	if cfg.ShopName == "" {
		return fmt.Errorf("shopName 不能为空")
	}
	if len(cfg.TableConf) == 0 {
		return fmt.Errorf("table-config 不能为空")
	}
	if len(cfg.ExcelData) == 0 {
		return fmt.Errorf("excel-data 不能为空")
	}
	if len(cfg.DbData.Lamps) == 0 {
		return fmt.Errorf("db-data.lamp 不能为空")
	}
	if len(cfg.FileData) == 0 {
		return fmt.Errorf("file-data 不能为空")
	}

	lamp := cfg.DbData.Lamps[0]
	if len(lamp.NumList) == 0 {
		return fmt.Errorf("编号列表解析失败或为空")
	}

	return nil
}