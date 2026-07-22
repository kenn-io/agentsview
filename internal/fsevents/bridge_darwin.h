#ifndef AGENTSVIEW_INTERNAL_FSEVENTS_BRIDGE_DARWIN_H
#define AGENTSVIEW_INTERNAL_FSEVENTS_BRIDGE_DARWIN_H

#include <CoreServices/CoreServices.h>
#include <dispatch/dispatch.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

typedef dispatch_queue_t AVFSEventsQueueRef;
typedef FSEventStreamRef AVFSEventsStreamRef;

AVFSEventsQueueRef avFSEventsQueueCreate(void);
void avFSEventsQueueRetain(AVFSEventsQueueRef queue);
void avFSEventsQueueRelease(AVFSEventsQueueRef queue);
void avFSEventsQueueDrain(AVFSEventsQueueRef queue);

AVFSEventsStreamRef avFSEventsStreamCreate(
    AVFSEventsQueueRef queue,
    const char *root,
    double latency,
    uintptr_t handle);
bool avFSEventsStreamStart(AVFSEventsStreamRef stream);
void avFSEventsStreamStop(AVFSEventsStreamRef stream);
void avFSEventsStreamInvalidate(AVFSEventsStreamRef stream);
void avFSEventsStreamRelease(AVFSEventsStreamRef stream);
void avFSEventsInvokeTestCallback(
    uintptr_t handle,
    size_t count,
    const char *path,
    uint32_t flags);

#endif
