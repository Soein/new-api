package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetSetupTestState 把测试涉及到的两张表清空，并保证 Setup 表结构存在。
// 复用 task_cas_test.go 中的 TestMain 提供的 in-memory SQLite。
func resetSetupTestState(t *testing.T) {
	t.Helper()
	require.NoError(t, DB.AutoMigrate(&Setup{}))
	require.NoError(t, DB.Exec("DELETE FROM setups").Error)
	require.NoError(t, DB.Exec("DELETE FROM users").Error)
}

// 删表用来稳定地制造一个 *非* ErrRecordNotFound 的查询错误，
// 模拟启动期 PG/HAProxy 抖动场景。t.Cleanup 负责恢复，不污染其它测试。
func breakSetupTable(t *testing.T) {
	t.Helper()
	require.NoError(t, DB.Migrator().DropTable(&Setup{}))
	t.Cleanup(func() {
		require.NoError(t, DB.AutoMigrate(&Setup{}))
	})
}

// ---------------------------------------------------------------------------
// GetSetup
// ---------------------------------------------------------------------------

func TestGetSetup_NoRecord_ReturnsNilNil(t *testing.T) {
	resetSetupTestState(t)

	setup, err := GetSetup()
	require.NoError(t, err)
	assert.Nil(t, setup)
}

func TestGetSetup_RecordExists_ReturnsSetup(t *testing.T) {
	resetSetupTestState(t)
	require.NoError(t, DB.Create(&Setup{Version: "v-test", InitializedAt: 12345}).Error)

	setup, err := GetSetup()
	require.NoError(t, err)
	require.NotNil(t, setup)
	assert.Equal(t, "v-test", setup.Version)
	assert.EqualValues(t, 12345, setup.InitializedAt)
}

func TestGetSetup_DBError_ReturnsError(t *testing.T) {
	resetSetupTestState(t)
	breakSetupTable(t)

	setup, err := GetSetup()
	require.Error(t, err, "DB 错误必须从 GetSetup 透传出来，不能被吞掉")
	assert.Nil(t, setup)
}

// ---------------------------------------------------------------------------
// CheckSetup
// ---------------------------------------------------------------------------

func TestCheckSetup_RecordExists_SetsConstantTrue(t *testing.T) {
	resetSetupTestState(t)
	constant.Setup = false

	require.NoError(t, DB.Create(&Setup{Version: "v", InitializedAt: 1}).Error)

	CheckSetup()
	assert.True(t, constant.Setup)
}

func TestCheckSetup_NoRecord_NoRootUser_SetsConstantFalse(t *testing.T) {
	resetSetupTestState(t)
	constant.Setup = true // 反向初始，验证逻辑会主动把它置 false

	CheckSetup()
	assert.False(t, constant.Setup)
}

func TestCheckSetup_NoRecord_RootUserExists_BackfillsAndSetsTrue(t *testing.T) {
	resetSetupTestState(t)
	constant.Setup = false

	require.NoError(t, DB.Create(&User{
		Id:       1,
		Username: "root",
		Role:     common.RoleRootUser,
		Status:   common.UserStatusEnabled,
	}).Error)

	CheckSetup()
	assert.True(t, constant.Setup)

	setup, err := GetSetup()
	require.NoError(t, err)
	require.NotNil(t, setup, "已存在 root 用户但缺 setup 记录时，CheckSetup 应自动补登记")
}

// 核心回归：DB 错误时 constant.Setup 必须保持原值不被污染。
// 这是修复要切断的攻击链——若已确认初始化的 true 被错误降级为 false，
// 未鉴权的 POST /api/setup 会重新对外开放。
func TestCheckSetup_DBError_DoesNotMutateConstant(t *testing.T) {
	resetSetupTestState(t)
	breakSetupTable(t)

	t.Run("preserves false when DB errors", func(t *testing.T) {
		constant.Setup = false
		CheckSetup()
		assert.False(t, constant.Setup, "DB 错误时不应改写状态")
	})

	t.Run("preserves true when DB errors (security-critical)", func(t *testing.T) {
		constant.Setup = true
		CheckSetup()
		assert.True(t, constant.Setup,
			"DB 错误时不应把已确认的 true 降级为 false，否则会重新打开未鉴权的 /api/setup POST")
	})
}
