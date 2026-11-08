//go:build darwin && cgo

package desktop

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#import <Cocoa/Cocoa.h>
#include <stdlib.h>

static NSString *coxVersion = nil;

// CoxMenuTarget handles the menu items AppKit has no standard action for.
@interface CoxMenuTarget : NSObject
- (void)about:(id)sender;
@end

@implementation CoxMenuTarget
- (void)about:(id)sender {
	[NSApp orderFrontStandardAboutPanelWithOptions:@{
		@"ApplicationName": @"Coxswain",
		@"ApplicationVersion": coxVersion ? coxVersion : @"",
		@"Version": @"",
	}];
	[NSApp activateIgnoringOtherApps:YES];
}
@end

static CoxMenuTarget *coxTarget = nil;

static NSMenuItem *coxItem(NSMenu *menu, NSString *title, SEL action, NSString *key, NSEventModifierFlags mods) {
	NSMenuItem *item = [menu addItemWithTitle:title action:action keyEquivalent:key];
	if (mods) {
		[item setKeyEquivalentModifierMask:mods];
	}
	return item;
}

// coxSetupMenu installs the standard main menu. Without it, ⌘C / ⌘V and
// friends never reach the web view. Edit items have no target, so they go
// to the first responder (the focused field in the page).
static void coxSetupMenu(const char *version) {
	@autoreleasepool {
		coxVersion = [[NSString alloc] initWithUTF8String:version];
		coxTarget = [[CoxMenuTarget alloc] init];
		NSMenu *bar = [[NSMenu alloc] init];

		NSMenu *app = [[NSMenu alloc] initWithTitle:@"Coxswain"];
		NSMenuItem *about = coxItem(app, @"About Coxswain", @selector(about:), @"", 0);
		[about setTarget:coxTarget];
		[app addItem:[NSMenuItem separatorItem]];
		coxItem(app, @"Hide Coxswain", @selector(hide:), @"h", 0);
		coxItem(app, @"Hide Others", @selector(hideOtherApplications:), @"h",
			NSEventModifierFlagOption | NSEventModifierFlagCommand);
		coxItem(app, @"Show All", @selector(unhideAllApplications:), @"", 0);
		[app addItem:[NSMenuItem separatorItem]];
		coxItem(app, @"Quit Coxswain", @selector(terminate:), @"q", 0);

		NSMenu *edit = [[NSMenu alloc] initWithTitle:@"Edit"];
		coxItem(edit, @"Undo", @selector(undo:), @"z", 0);
		coxItem(edit, @"Redo", @selector(redo:), @"z", NSEventModifierFlagShift | NSEventModifierFlagCommand);
		[edit addItem:[NSMenuItem separatorItem]];
		coxItem(edit, @"Cut", @selector(cut:), @"x", 0);
		coxItem(edit, @"Copy", @selector(copy:), @"c", 0);
		coxItem(edit, @"Paste", @selector(paste:), @"v", 0);
		coxItem(edit, @"Select All", @selector(selectAll:), @"a", 0);

		NSMenu *window = [[NSMenu alloc] initWithTitle:@"Window"];
		coxItem(window, @"Minimize", @selector(performMiniaturize:), @"m", 0);
		coxItem(window, @"Close", @selector(performClose:), @"w", 0);

		for (NSMenu *m in @[app, edit, window]) {
			NSMenuItem *top = [[NSMenuItem alloc] initWithTitle:[m title] action:nil keyEquivalent:@""];
			[top setSubmenu:m];
			[bar addItem:top];
		}
		[NSApp setMainMenu:bar];
		[NSApp setWindowsMenu:window];
	}
}
*/
import "C"

import "unsafe"

// setupMenu installs the macOS main menu (Coxswain, Edit, Window). It
// must run on the main thread, after the window exists.
func setupMenu(version string) {
	v := C.CString(version)
	defer C.free(unsafe.Pointer(v))
	C.coxSetupMenu(v)
}
