#include "bridge_darwin.h"

extern void goFSEventsCallback(
    uintptr_t handle,
    size_t count,
    char **paths,
    uint32_t *flags,
    uint64_t *ids);

static void avFSEventsCallback(
    ConstFSEventStreamRef stream,
    void *context,
    size_t count,
    void *eventPaths,
    const FSEventStreamEventFlags *eventFlags,
    const FSEventStreamEventId *eventIDs) {
  (void)stream;
  goFSEventsCallback(
      (uintptr_t)context,
      count,
      (char **)eventPaths,
      (uint32_t *)eventFlags,
      (uint64_t *)eventIDs);
}

static void avFSEventsDrainCallback(void *context) {
  (void)context;
}

AVFSEventsQueueRef avFSEventsQueueCreate(void) {
  return dispatch_queue_create(
      "agentsview.fsevents", DISPATCH_QUEUE_SERIAL);
}

void avFSEventsQueueRetain(AVFSEventsQueueRef queue) {
  dispatch_retain(queue);
}

void avFSEventsQueueRelease(AVFSEventsQueueRef queue) {
  dispatch_release(queue);
}

void avFSEventsQueueDrain(AVFSEventsQueueRef queue) {
  dispatch_sync_f(queue, NULL, avFSEventsDrainCallback);
}

AVFSEventsStreamRef avFSEventsStreamCreate(
    AVFSEventsQueueRef queue,
    const char *root,
    double latency,
    uintptr_t handle) {
  CFStringRef path = CFStringCreateWithFileSystemRepresentation(
      kCFAllocatorDefault, root);
  if (path == NULL) {
    return NULL;
  }

  const void *values[] = {path};
  CFArrayRef paths = CFArrayCreate(
      kCFAllocatorDefault, values, 1, &kCFTypeArrayCallBacks);
  if (paths == NULL) {
    CFRelease(path);
    return NULL;
  }

  FSEventStreamContext context = {
      .version = 0,
      .info = (void *)handle,
      .retain = NULL,
      .release = NULL,
      .copyDescription = NULL,
  };
  FSEventStreamCreateFlags flags =
      kFSEventStreamCreateFlagFileEvents |
      kFSEventStreamCreateFlagWatchRoot;
  FSEventStreamRef stream = FSEventStreamCreate(
      kCFAllocatorDefault,
      avFSEventsCallback,
      &context,
      paths,
      kFSEventStreamEventIdSinceNow,
      latency,
      flags);

  CFRelease(paths);
  CFRelease(path);
  if (stream == NULL) {
    return NULL;
  }

  FSEventStreamSetDispatchQueue(stream, queue);
  return stream;
}

bool avFSEventsStreamStart(AVFSEventsStreamRef stream) {
  return FSEventStreamStart(stream);
}

void avFSEventsStreamStop(AVFSEventsStreamRef stream) {
  FSEventStreamStop(stream);
}

void avFSEventsStreamInvalidate(AVFSEventsStreamRef stream) {
  FSEventStreamInvalidate(stream);
}

void avFSEventsStreamRelease(AVFSEventsStreamRef stream) {
  FSEventStreamRelease(stream);
}

void avFSEventsInvokeTestCallback(
    uintptr_t handle,
    size_t count,
    const char *path,
    uint32_t flags) {
  char **paths = calloc(count, sizeof(char *));
  uint32_t *eventFlags = calloc(count, sizeof(uint32_t));
  uint64_t *eventIDs = calloc(count, sizeof(uint64_t));
  if (paths == NULL || eventFlags == NULL || eventIDs == NULL) {
    free(paths);
    free(eventFlags);
    free(eventIDs);
    return;
  }
  for (size_t i = 0; i < count; i++) {
    paths[i] = (char *)path;
    eventFlags[i] = flags;
    eventIDs[i] = i + 1;
  }
  goFSEventsCallback(handle, count, paths, eventFlags, eventIDs);
  free(paths);
  free(eventFlags);
  free(eventIDs);
}
