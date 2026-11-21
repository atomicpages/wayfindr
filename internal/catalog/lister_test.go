package catalog_test

import (
	"github.com/atomicpages/wayfindr/internal/catalog"
	"github.com/atomicpages/wayfindr/internal/embed"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// The daemon passes its upstream.Manager as the Syncer's lister.
var _ catalog.ToolLister = (*upstream.Manager)(nil)

// The daemon passes its embed.Client as the Syncer's embedder.
var _ catalog.Embedder = (*embed.Client)(nil)

// catalog recognises upstream's unknown_connector failures by this method,
// since it may not import upstream.
var _ interface{ UpstreamKind() string } = (*upstream.Error)(nil)
