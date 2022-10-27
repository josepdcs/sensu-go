// Code generated by interfacer; DO NOT EDIT

package mockapi

import (
	"context"
	"github.com/sensu/core/v2"
)

// HandlerClient is an interface generated for "github.com/sensu/sensu-go/backend/api.HandlerClient".
type HandlerClient interface {
	CreateHandler(context.Context, *v2.Handler) error
	DeleteHandler(context.Context, string) error
	FetchHandler(context.Context, string) (*v2.Handler, error)
	ListHandlers(context.Context) ([]*v2.Handler, error)
	UpdateHandler(context.Context, *v2.Handler) error
}