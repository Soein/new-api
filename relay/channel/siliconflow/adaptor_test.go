package siliconflow

import (
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConvertImageRequest_BatchSizeBound(t *testing.T) {
	a := &Adaptor{}

	ptrUint := func(v uint) *uint { return &v }

	tests := []struct {
		name    string
		request dto.ImageRequest
		wantErr bool
	}{
		{
			name: "extra batch_size within bound",
			request: dto.ImageRequest{
				Model: "test-model",
				Extra: map[string]json.RawMessage{
					"batch_size": json.RawMessage("4"),
				},
			},
			wantErr: false,
		},
		{
			name: "extra batch_size exceeds MaxImageN, bypassing the top-level n bound",
			request: dto.ImageRequest{
				Model: "test-model",
				Extra: map[string]json.RawMessage{
					"batch_size": json.RawMessage("999999"),
				},
			},
			wantErr: true,
		},
		{
			name: "extra batch_size exactly at MaxImageN",
			request: dto.ImageRequest{
				Model: "test-model",
				Extra: map[string]json.RawMessage{
					"batch_size": json.RawMessage("128"),
				},
			},
			wantErr: false,
		},
		{
			name: "falls back to bounded top-level n when extra has no batch_size",
			request: dto.ImageRequest{
				Model: "test-model",
				N:     ptrUint(2),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := a.ConvertImageRequest(nil, nil, tt.request)
			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, result)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, result)
		})
	}
}
