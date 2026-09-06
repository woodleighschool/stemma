package icon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

func render(ctx context.Context, source string, size int) ([]byte, string, string, error) {
	if !strings.EqualFold(filepath.Ext(source), ".app") {
		return nil, "", "", errors.New("system icon rendering requires a .app bundle")
	}
	info, err := os.Stat(source)
	if err != nil {
		return nil, "", "", err
	}
	if !info.IsDir() {
		return nil, "", "", errors.New("system icon source must be a .app directory")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, err := quickLookPNG(ctx, source, size)
	if err != nil {
		return nil, "", "", fmt.Errorf("quick look icon rendering: %w", err)
	}
	version, err := exec.CommandContext(ctx, "/usr/bin/sw_vers", "-productVersion").Output()
	if err != nil {
		return nil, "", "", err
	}
	build, err := exec.CommandContext(ctx, "/usr/bin/sw_vers", "-buildVersion").Output()
	if err != nil {
		return nil, "", "", err
	}
	return data, strings.TrimSpace(string(version)), strings.TrimSpace(string(build)), nil
}

type quickLookAPI struct {
	destination func(objc.ID, objc.ID, uintptr, uintptr) uintptr
	addImage    func(uintptr, uintptr, uintptr)
	finalize    func(uintptr) bool
	release     func(uintptr)
}

var loadQuickLook = sync.OnceValues(func() (*quickLookAPI, error) {
	libraries := make(map[string]uintptr)
	for _, name := range []string{"Foundation", "QuickLookThumbnailing", "ImageIO", "CoreFoundation"} {
		handle, err := purego.Dlopen("/System/Library/Frameworks/"+name+".framework/"+name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			return nil, err
		}
		libraries[name] = handle
	}
	api := &quickLookAPI{}
	for _, binding := range []struct {
		function any
		library  string
		symbol   string
	}{
		{&api.destination, "ImageIO", "CGImageDestinationCreateWithData"},
		{&api.addImage, "ImageIO", "CGImageDestinationAddImage"},
		{&api.finalize, "ImageIO", "CGImageDestinationFinalize"},
		{&api.release, "CoreFoundation", "CFRelease"},
	} {
		symbol, err := purego.Dlsym(libraries[binding.library], binding.symbol)
		if err != nil {
			return nil, err
		}
		purego.RegisterFunc(binding.function, symbol)
	}
	return api, nil
})

func message(object objc.ID, selector string, arguments ...any) objc.ID {
	return object.Send(objc.RegisterName(selector), arguments...)
}

func nativeClass(name string) objc.ID { return objc.ID(objc.GetClass(name)) }

func quickLookPNG(ctx context.Context, source string, size int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	api, err := loadQuickLook()
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
			output.data, output.err = api.png(thumbnail)
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

func (api *quickLookAPI) png(thumbnail objc.ID) ([]byte, error) {
	image := uintptr(message(thumbnail, "CGImage"))
	if image == 0 {
		return nil, errors.New("quick look returned no bitmap")
	}
	data := message(nativeClass("NSMutableData"), "new")
	defer message(data, "release")
	format := message(nativeClass("NSString"), "stringWithUTF8String:", "public.png")
	destination := api.destination(data, format, 1, 0)
	if destination == 0 {
		return nil, errors.New("cannot create PNG destination")
	}
	defer api.release(destination)
	api.addImage(destination, image, 0)
	if !api.finalize(destination) {
		return nil, errors.New("cannot encode Quick Look PNG")
	}
	length := uintptr(message(data, "length"))
	if length == 0 || length > maxImageBytes {
		return nil, errors.New("quick look PNG has invalid size")
	}
	pointer := objc.Send[unsafe.Pointer](data, objc.RegisterName("bytes"))
	if pointer == nil {
		return nil, errors.New("quick look returned no PNG data")
	}
	return append([]byte(nil), unsafe.Slice((*byte)(pointer), int(length))...), nil
}
