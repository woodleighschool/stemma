package icon

import (
	"context"
	"errors"
	"runtime"
	"sync"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

var loadQuickLook = sync.OnceValue(func() error {
	for _, name := range []string{"Foundation", "QuickLookThumbnailing"} {
		if _, err := purego.Dlopen("/System/Library/Frameworks/"+name+".framework/"+name, purego.RTLD_NOW|purego.RTLD_GLOBAL); err != nil {
			return err
		}
	}
	return nil
})

func message(object objc.ID, selector string, arguments ...any) objc.ID {
	return object.Send(objc.RegisterName(selector), arguments...)
}

func nativeClass(name string) objc.ID { return objc.ID(objc.GetClass(name)) }

func quickLookPNG(ctx context.Context, source string, size int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := loadQuickLook(); err != nil {
		return nil, err
	}
	api, err := loadImageIO()
	if err != nil {
		return nil, err
	}
	// Autorelease pools belong to an OS thread, including across the asynchronous wait.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pool := message(nativeClass("NSAutoreleasePool"), "new")
	defer message(pool, "drain")

	path := message(nativeClass("NSString"), "stringWithUTF8String:", source)
	url := message(nativeClass("NSURL"), "fileURLWithPath:", path)
	dimensions := struct{ Width, Height float64 }{float64(size), float64(size)}
	request := message(message(nativeClass("QLThumbnailGenerationRequest"), "alloc"), "initWithFileAtURL:size:scale:representationTypes:", url, dimensions, float64(1), uintptr(1))
	if request == 0 {
		return nil, errors.New("cannot create Quick Look request")
	}
	defer message(request, "release")
	message(request, "setIconMode:", true)
	generator := message(nativeClass("QLThumbnailGenerator"), "sharedGenerator")
	if generator == 0 {
		return nil, errors.New("cannot create Quick Look generator")
	}
	type result struct {
		data []byte
		err  error
	}
	completed := make(chan result, 1)
	block := objc.NewBlock(func(_ objc.Block, thumbnail objc.ID, failure objc.ID) {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		pool := message(nativeClass("NSAutoreleasePool"), "new")
		defer message(pool, "drain")
		var output result
		if failure != 0 {
			description := message(failure, "localizedDescription")
			output.err = errors.New(objc.Send[string](description, objc.RegisterName("UTF8String")))
		} else {
			output.data, output.err = api.png(uintptr(message(thumbnail, "CGImage")))
		}
		// A late callback can finish after context cancellation without blocking.
		select {
		case completed <- output:
		default:
		}
	})
	// Quick Look owns its copy; releasing ours also permits late completion after cancellation.
	defer block.Release()
	message(generator, "generateBestRepresentationForRequest:completionHandler:", request, block)
	defer message(generator, "cancelRequest:", request)
	select {
	case output := <-completed:
		return output.data, output.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
