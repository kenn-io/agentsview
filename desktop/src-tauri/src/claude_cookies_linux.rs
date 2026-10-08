use webkit2gtk::{gio, glib, CookieManager, CookieManagerExt};
use glib::translate::{from_glib_full, FromGlibPtrContainer, ToGlibPtr};

type Completion = tokio::sync::oneshot::Sender<Result<(), String>>;

// get_all_cookies is absent from the generated Rust bindings.
extern "C" {
    fn webkit_cookie_manager_get_all_cookies(manager: *mut webkit2gtk::ffi::WebKitCookieManager, cancellable: *mut gio::ffi::GCancellable, callback: gio::ffi::GAsyncReadyCallback, data: glib::ffi::gpointer);
    fn webkit_cookie_manager_get_all_cookies_finish(manager: *mut webkit2gtk::ffi::WebKitCookieManager, result: *mut gio::ffi::GAsyncResult, error: *mut *mut glib::ffi::GError) -> *mut glib::ffi::GList;
}

pub fn delete_cookies(manager: CookieManager, sender: Completion) {
    unsafe extern "C" fn received(source: *mut glib::gobject_ffi::GObject, result: *mut gio::ffi::GAsyncResult, data: glib::ffi::gpointer) {
        let (manager, sender) = *Box::from_raw(data.cast::<(CookieManager, Completion)>());
        let mut error = std::ptr::null_mut();
        let cookies = webkit_cookie_manager_get_all_cookies_finish(source.cast(), result, &mut error);
        if !error.is_null() {
            let error: glib::Error = from_glib_full(error);
            let _ = sender.send(Err(error.to_string()));
            return;
        }
        let cookies: Vec<soup::Cookie> = FromGlibPtrContainer::from_glib_full(cookies);
        glib::MainContext::ref_thread_default().spawn_local(async move {
            for mut cookie in cookies {
                let domain = cookie.domain().map(|domain| domain.to_string()).unwrap_or_default();
                if domain == "claude.ai" || domain.ends_with(".claude.ai") {
                    if let Err(error) = manager.delete_cookie_future(&mut cookie).await {
                        let _ = sender.send(Err(error.to_string()));
                        return;
                    }
                }
            }
            let _ = sender.send(Ok(()));
        });
    }
    unsafe {
        let pointer = manager.to_glib_none().0;
        webkit_cookie_manager_get_all_cookies(pointer, std::ptr::null_mut(), Some(received), Box::into_raw(Box::new((manager, sender))).cast());
    }
}
