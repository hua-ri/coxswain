//go:build darwin && cgo

package desktop

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#import <Cocoa/Cocoa.h>
#include <stdlib.h>

// coxPickFile shows a modal "open file" panel and returns the chosen
// file's path (malloc'd; the caller frees it), or NULL if cancelled.
static char *coxPickFile(void) {
	@autoreleasepool {
		NSOpenPanel *panel = [NSOpenPanel openPanel];
		[panel setCanChooseFiles:YES];
		[panel setCanChooseDirectories:NO];
		[panel setAllowsMultipleSelection:NO];
		[panel setResolvesAliases:YES];
		[panel setMessage:@"Choose a file to upload"];
		[panel setPrompt:@"Upload"];
		if ([panel runModal] != NSModalResponseOK) {
			return NULL;
		}
		NSURL *url = [[panel URLs] firstObject];
		if (url == nil || ![url isFileURL]) {
			return NULL;
		}
		return strdup([[url path] fileSystemRepresentation]);
	}
}
*/
import "C"

import "unsafe"

// nativePicker shows the macOS open panel. It must run on the main thread.
func nativePicker() string {
	p := C.coxPickFile()
	if p == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(p))
	return C.GoString(p)
}

var pickFile = nativePicker
