package icon

import (
	"errors"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

type imageIOAPI struct {
	destination func(objc.ID, objc.ID, uintptr, uintptr) uintptr
	addImage    func(uintptr, uintptr, uintptr)
	finalize    func(uintptr) bool
	release     func(uintptr)
}

var loadImageIO = sync.OnceValues(func() (*imageIOAPI, error) {
	libraries := make(map[string]uintptr)
	for _, name := range []string{"ImageIO", "CoreFoundation"} {
		handle, err := purego.Dlopen("/System/Library/Frameworks/"+name+".framework/"+name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			return nil, err
		}
		libraries[name] = handle
	}
	api := &imageIOAPI{}
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

func (api *imageIOAPI) png(image uintptr) ([]byte, error) {
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
