package warranty

import "errors"

// 本包使用的错误。调用方可以用 errors.Is 判断具体失败原因。
var (
	// ErrDuplicateID 表示登记时使用了已存在的编号，原记录保留。
	ErrDuplicateID = errors.New("warranty: duplicate id")
	// ErrNotFound 表示引用的产品、备件、请求或承诺不存在。
	ErrNotFound = errors.New("warranty: not found")
	// ErrInvalidParam 表示入参不合法（如非正整数数量、缺少故障代码）。
	ErrInvalidParam = errors.New("warranty: invalid parameter")
	// ErrConflict 表示同一编号被重复用于不同内容。
	ErrConflict = errors.New("warranty: conflict")
	// ErrIneligible 表示请求在当次时刻不满足保修资格。
	ErrIneligible = errors.New("warranty: request is not eligible")
	// ErrInsufficientStock 表示可承诺数量不足。
	ErrInsufficientStock = errors.New("warranty: insufficient committable stock")
	// ErrCommitmentClosed 表示承诺已取消或已到期，不能继续使用。
	ErrCommitmentClosed = errors.New("warranty: commitment is no longer active")
	// ErrUsageExceeded 表示本次使用数量超过该承诺的未用数量，整次失败。
	ErrUsageExceeded = errors.New("warranty: usage exceeds unused quantity")
)
