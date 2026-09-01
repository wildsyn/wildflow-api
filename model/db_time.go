package model

import (
	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

// GetDBTimestamp returns a UNIX timestamp from database time.
// Falls back to application time on error.
func GetDBTimestamp() int64 {
	return getDBTimestampOn(DB)
}

// getDBTimestampOn 在给定连接上读取数据库时钟：事务内应传入 tx 以使用同一
// 连接（生产上避免额外占用连接池，测试中避免单连接池死锁），并保证事务内
// 时钟一致。
func getDBTimestampOn(db *gorm.DB) int64 {
	var ts int64
	var err error
	switch {
	case common.UsingMainDatabase(common.DatabaseTypePostgreSQL):
		err = db.Raw("SELECT EXTRACT(EPOCH FROM NOW())::bigint").Scan(&ts).Error
	case common.UsingMainDatabase(common.DatabaseTypeSQLite):
		err = db.Raw("SELECT strftime('%s','now')").Scan(&ts).Error
	default:
		err = db.Raw("SELECT UNIX_TIMESTAMP()").Scan(&ts).Error
	}
	if err != nil || ts <= 0 {
		return common.GetTimestamp()
	}
	return ts
}
