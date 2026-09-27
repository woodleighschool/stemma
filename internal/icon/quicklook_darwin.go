package icon

import (
	"context"
	"errors"

	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/corefoundation"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/imageio"
	ql "github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/quicklookthumbnailing"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/runtime/obj"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/runtime/purego"
	"github.com/ebitengine/purego/objc"
)

// quickLookPNG has Quick Look draw the icon of source at size pixels and
// returns it as PNG.
func quickLookPNG(ctx context.Context, source string, size int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request := ql.NewThumbnailGenerationRequestWithFileAtURLSizeScaleRepresentationTypes(
		source, corefoundation.CGSize{Width: float64(size), Height: float64(size)}, 1,
		ql.ThumbnailGenerationRequestRepresentationTypeIcon,
	).WithIconMode(true)
	generator := ql.SharedGenerator()
	// Stops the work a cancelled context abandons; a no-op once Quick Look is done.
	defer generator.CancelRequest(request)
	type result struct {
		thumbnail *ql.ThumbnailRepresentation
		err       error
	}
	completed := make(chan result, 1)
	block := objc.NewBlock(func(_ objc.Block, thumbnail, failure objc.ID) {
		completed <- result{ql.ThumbnailRepresentationFromID(thumbnail), purego.NSErrorToError(failure)}
	})
	// The binding's async wrapper does not release its block. Own this one;
	// Quick Look retains its copy until any late completion after cancellation.
	defer block.Release()
	objc.Send[objc.ID](obj.ID(generator), objc.RegisterName("generateBestRepresentationForRequest:completionHandler:"), obj.ID(request), block)
	var thumbnail *ql.ThumbnailRepresentation
	select {
	case result := <-completed:
		if result.err != nil {
			return nil, result.err
		}
		thumbnail = result.thumbnail
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if thumbnail == nil {
		return nil, errors.New("quick look returned no icon")
	}
	bitmap := thumbnail.CGImage()
	if bitmap.IsNil() {
		return nil, errors.New("quick look returned no bitmap")
	}
	buffer := corefoundation.CFDataCreateMutable(corefoundation.CFAllocatorRef{}, 0)
	format := corefoundation.CFStringCreateWithCString(corefoundation.CFAllocatorRef{}, "public.png", int(corefoundation.KCFStringEncodingUTF8))
	destination := imageio.CGImageDestinationCreateWithData(buffer, format, 1, corefoundation.CFDictionaryRef{})
	if destination.IsNil() {
		return nil, errors.New("cannot create PNG destination")
	}
	imageio.CGImageDestinationAddImage(destination, bitmap, corefoundation.CFDictionaryRef{})
	if !imageio.CGImageDestinationFinalize(destination) {
		return nil, errors.New("cannot encode Quick Look PNG")
	}
	data := obj.Bytes(buffer)
	if len(data) == 0 {
		return nil, errors.New("quick look returned no PNG data")
	}
	return data, nil
}
