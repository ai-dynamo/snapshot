// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Pause a real CUDA unbind after the driver has finished, before the shim can
// record its result. No CUDA result or operation is replaced by this fixture.
#include <cupti_callbacks.h>
#include <cupti_driver_cbid.h>
#include <errno.h>
#include <pthread.h>
#include <time.h>

static CUpti_SubscriberHandle subscriber;
static pthread_mutex_t mutex = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t changed = PTHREAD_COND_INITIALIZER;
static int entered, released, bind_entered, unbind_status;

static void CUPTIAPI callback(void *userdata, CUpti_CallbackDomain domain,
                             CUpti_CallbackId id, const void *data) {
    (void)userdata;
    if (domain != CUPTI_CB_DOMAIN_DRIVER_API)
        return;
    const CUpti_CallbackData *event = data;
    pthread_mutex_lock(&mutex);
    if (id == CUPTI_DRIVER_TRACE_CBID_cuMulticastBindMem &&
        event->callbackSite == CUPTI_API_ENTER) {
        bind_entered = 1;
        pthread_cond_broadcast(&changed);
    }
    if (id == CUPTI_DRIVER_TRACE_CBID_cuMulticastUnbind &&
        event->callbackSite == CUPTI_API_EXIT && !entered) {
        unbind_status = *(const CUresult *)event->functionReturnValue;
        entered = 1;
        pthread_cond_broadcast(&changed);
        while (!released)
            pthread_cond_wait(&changed, &mutex);
    }
    pthread_mutex_unlock(&mutex);
}

int multicast_gate_start(void) {
    CUptiResult result = cuptiSubscribe(&subscriber, callback, NULL);
    if (result != CUPTI_SUCCESS)
        return result;
    result = cuptiEnableCallback(1, subscriber, CUPTI_CB_DOMAIN_DRIVER_API,
                                CUPTI_DRIVER_TRACE_CBID_cuMulticastUnbind);
    if (result != CUPTI_SUCCESS)
        return result;
    return cuptiEnableCallback(1, subscriber, CUPTI_CB_DOMAIN_DRIVER_API,
                               CUPTI_DRIVER_TRACE_CBID_cuMulticastBindMem);
}

// kind 0 waits for native unbind's exit, kind 1 for native bind's entry.
int multicast_gate_wait(int kind, int timeout_ms) {
    struct timespec deadline;
    clock_gettime(CLOCK_REALTIME, &deadline);
    deadline.tv_sec += timeout_ms / 1000;
    deadline.tv_nsec += (timeout_ms % 1000) * 1000000L;
    if (deadline.tv_nsec >= 1000000000L) {
        deadline.tv_sec++;
        deadline.tv_nsec -= 1000000000L;
    }
    pthread_mutex_lock(&mutex);
    int *ready = kind ? &bind_entered : &entered;
    while (!*ready) {
        int result = pthread_cond_timedwait(&changed, &mutex, &deadline);
        if (result == ETIMEDOUT)
            break;
    }
    int result = *ready;
    pthread_mutex_unlock(&mutex);
    return result;
}

int multicast_gate_status(void) {
    pthread_mutex_lock(&mutex);
    int result = unbind_status;
    pthread_mutex_unlock(&mutex);
    return result;
}

void multicast_gate_release(void) {
    pthread_mutex_lock(&mutex);
    released = 1;
    pthread_cond_broadcast(&changed);
    pthread_mutex_unlock(&mutex);
}

int multicast_gate_stop(void) {
    return cuptiUnsubscribe(subscriber);
}
