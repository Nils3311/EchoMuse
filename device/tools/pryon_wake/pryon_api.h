/*
 * Subset of the C API of Amazon's libpryon.so, as recovered by the echo-dot-assist
 * project (https://github.com/Gamer92000/echo-dot-assist, src/include/pryon_api.h,
 * MIT License, Copyright (c) 2026 Julian Imhof). Only declarations and structure
 * layouts are reproduced here; no Amazon code. The layouts are for 32-bit ARM
 * (Fire OS, armv7) and were confirmed by running the stock library there.
 *
 * pryon_wake never links against libpryon.so. It dlopen()s the copy that is
 * already on the device (/system/lib/libpryon.so) and looks these symbols up.
 */
#ifndef PRYON_API_H
#define PRYON_API_H

#include <stdint.h>

#define PRYON_MAX_CHANNELS 32

/* 0x90 bytes, passed BY VALUE to the decoder constructor.
 * Default = { 0, 16000, 16, 1, { 0, 5, 5, ... 5 } }. */
typedef struct PryonMultichannelAudioFormat {
    int32_t encoding;
    int32_t sampleRate;
    int32_t bitsPerSample;
    int32_t numChannels;
    int32_t channelTypes[PRYON_MAX_CHANNELS];
} PryonMultichannelAudioFormat;

/* Partial layout. Read through the pointer inside the callback only; never
 * allocate or copy it. */
typedef struct _PryonEnumeratedResult {
    uint8_t     _unknown00[0x08];
    uint64_t    beginSampleIndex;   /* keyword start, in pushed-sample index space */
    uint8_t     _unknown10[0x0c];
    const char *keyword;            /* e.g. "ALEXA", "STOP" */
    uint8_t     _unknown20[0x04];
    int32_t     detectionType;      /* 0 = NearMiss, 2 = Accept (3 = flagged variant, not seen) */
    uint32_t    metadataSize;
    const void *metadata;
    uint8_t     _unknown30[0x10];
    uint64_t    endSampleIndex;     /* keyword end */
} PryonEnumeratedResult;

#define PRYON_DETECTION_TYPE_NEAR_MISS 0
#define PRYON_DETECTION_TYPE_ACCEPT    2
#define PRYON_DETECTION_TYPE_ACCEPT_FLAGGED 3

typedef void (*PryonLoggingCallback)(int level, const char *tag, const char *message);
typedef void (*PryonEnumeratedResultCallback)(const char *decoderId, PryonEnumeratedResult *result);

/* A client property: the model lowers its accept threshold while one of the
 * names it knows (AlarmState, AudioPlayerState, audio_playback) equals 1. */
typedef struct PryonClientProperty {
    const char *name;
    uint32_t    _pad;
    int64_t     value;
} PryonClientProperty;

typedef struct PryonClientEvent {
    uint32_t                   one;      /* must be 1 */
    const PryonClientProperty *property;
} PryonClientEvent;

#endif /* PRYON_API_H */
