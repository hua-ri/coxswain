//go:build darwin && cgo

package desktop

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#import <Cocoa/Cocoa.h>
#include <stdlib.h>

// coxPickFile shows a modal "open file" panel and returns the chosen
// file's path (malloc'd; the caller frees it), or NULL if cancelled.
// purpose: 0 a file to upload, 1 an SSH private key (starts in ~/.ssh,
// shows hidden files), 2 a kubeconfig (starts in ~/.kube, hidden files).
static char *coxPickFile(int purpose) {
	@autoreleasepool {
		NSOpenPanel *panel = [NSOpenPanel openPanel];
		[panel setCanChooseFiles:YES];
		[panel setCanChooseDirectories:NO];
		[panel setAllowsMultipleSelection:NO];
		[panel setResolvesAliases:YES];
		NSString *home = NSHomeDirectory();
		switch (purpose) {
		case 1:
			[panel setMessage:@"Choose a private key (the key stays where it is; Coxswain only remembers its path)"];
			[panel setPrompt:@"Use Key"];
			[panel setShowsHiddenFiles:YES];
			[panel setDirectoryURL:[NSURL fileURLWithPath:[home stringByAppendingPathComponent:@".ssh"]]];
			break;
		case 2:
			[panel setMessage:@"Choose a kubeconfig file (it is only read)"];
			[panel setPrompt:@"Choose"];
			[panel setShowsHiddenFiles:YES];
			[panel setDirectoryURL:[NSURL fileURLWithPath:[home stringByAppendingPathComponent:@".kube"]]];
			break;
		default:
			[panel setMessage:@"Choose a file to upload"];
			[panel setPrompt:@"Upload"];
		}
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
func nativePicker(purpose string) string {
	n := map[string]C.int{"key": 1, "kubeconfig": 2}[purpose]
	p := C.coxPickFile(n)
	if p == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(p))
	return C.GoString(p)
}

var pickFile = nativePicker
