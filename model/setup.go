package model

import (
	"errors"

	"gorm.io/gorm"
)

type Setup struct {
	ID            uint   `json:"id" gorm:"primaryKey"`
	Version       string `json:"version" gorm:"type:varchar(50);not null"`
	InitializedAt int64  `json:"initialized_at" gorm:"type:bigint;not null"`
}

// GetSetup 返回 setup 记录。
// 约定：
//   - 记录不存在 → (nil, nil)
//   - 数据库错误 → (nil, err)，调用方需自行决定是否信任后续状态
func GetSetup() (*Setup, error) {
	var setup Setup
	err := DB.First(&setup).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &setup, nil
}
