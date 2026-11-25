// Package bootonly 提供框架内部的装配标记。
package bootonly

// AuthRoutes 让 boot 可跨包挂载鉴权端点,但不向业务开放手动挂载。
type AuthRoutes struct{ bootOnly struct{} }
