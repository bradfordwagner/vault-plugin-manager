package main

import (
	"context"

	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/logical"
)

// Factory builds the e2e test backend: a trivial key/value secrets engine that
// stores whatever fields are written at a path and returns them on read. It is
// only used to prove the plugin manager can register a plugin and stand up a
// working mount across Vault versions.
func Factory(ctx context.Context, conf *logical.BackendConfig) (logical.Backend, error) {
	b := &framework.Backend{
		Help:        "e2e test secrets plugin: stores and returns arbitrary key/values",
		BackendType: logical.TypeLogical,
		Paths: []*framework.Path{
			{
				Pattern: ".*",
				// Only UpdateOperation for writes: it handles both create and
				// update, so we avoid needing an ExistenceCheck callback.
				Operations: map[logical.Operation]framework.OperationHandler{
					logical.UpdateOperation: &framework.PathOperation{Callback: handleWrite},
					logical.ReadOperation:   &framework.PathOperation{Callback: handleRead},
					logical.DeleteOperation: &framework.PathOperation{Callback: handleDelete},
				},
			},
		},
	}
	if err := b.Setup(ctx, conf); err != nil {
		return nil, err
	}
	return b, nil
}

func handleWrite(ctx context.Context, req *logical.Request, _ *framework.FieldData) (*logical.Response, error) {
	entry, err := logical.StorageEntryJSON(req.Path, req.Data)
	if err != nil {
		return nil, err
	}
	if err := req.Storage.Put(ctx, entry); err != nil {
		return nil, err
	}
	return nil, nil
}

func handleRead(ctx context.Context, req *logical.Request, _ *framework.FieldData) (*logical.Response, error) {
	entry, err := req.Storage.Get(ctx, req.Path)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, nil
	}
	var data map[string]interface{}
	if err := entry.DecodeJSON(&data); err != nil {
		return nil, err
	}
	return &logical.Response{Data: data}, nil
}

func handleDelete(ctx context.Context, req *logical.Request, _ *framework.FieldData) (*logical.Response, error) {
	return nil, req.Storage.Delete(ctx, req.Path)
}
