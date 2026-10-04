/*
 * Stand-in for libpryon.so, for testing pryon_wake's protocol on a host. It
 * exports the same symbols and fires a result when it hears a loud burst:
 *   |sample| >= 20000 for 400+ samples -> keyword STOP, accept
 *   |sample| >=  4000 for 400+ samples -> keyword ALEXA, accept
 *   |sample| >=  1500 for 400+ samples -> keyword ALEXA, near miss
 * STUB_BAD=1 makes it report a keyword end far beyond the fed audio (what a
 * library with a different structure layout would look like to pryon_wake).
 */
#include <stdlib.h>
#include <stdio.h>
#include <string.h>
#include "../pryon_api.h"

static PryonLoggingCallback logcb;
static PryonEnumeratedResultCallback rescb;
static int run;
static int level;
static uint64_t run_start;

const char *PryonApi_GetAttributes(void) { return "{\"stub\":true}"; }
int PryonApi_SetLoggingCallback(PryonLoggingCallback cb) { logcb = cb; return 0; }
int PryonApi_SetEnumeratedResultCallback(PryonEnumeratedResultCallback cb) { rescb = cb; return 0; }
int PryonModelSet_New(const char *id, const char *manifest, const char *s) {
    (void)id; (void)s; return strstr(manifest, "missing") ? 1 : 0; }
int PryonModelSet_Delete(const char *id) { (void)id; return 0; }
void PryonDecoder_NewPryonMultichannelAudioFormat_Default(PryonMultichannelAudioFormat *f) {
    memset(f, 0, sizeof *f); f->sampleRate = 16000; f->bitsPerSample = 16; f->numChannels = 1; }
int PryonDecoder_NewSpotterAudioDecoder(const char *a, const char *b, const char *c,
                                        PryonMultichannelAudioFormat f, const char *j) {
    (void)a; (void)b; (void)c; (void)f; (void)j; return 0; }
int PryonDecoder_SessionEnd(const char *id) { (void)id; fprintf(stderr, "stub: SessionEnd\n"); return 0; }
int PryonDecoder_PushClientEvents(const char *id, const PryonClientEvent *ev, uint32_t n) {
    (void)id;
    for (uint32_t i = 0; i < n; i++)
        fprintf(stderr, "stub: property %s=%lld\n", ev[i].property->name, (long long)ev[i].property->value);
    return 0; }
int PryonDecoder_Delete(const char *id) { (void)id; return 0; }

static void fire(const char *kw, int type, uint64_t b, uint64_t e) {
    PryonEnumeratedResult r;
    memset(&r, 0, sizeof r);
    r.keyword = kw; r.detectionType = type; r.beginSampleIndex = b; r.endSampleIndex = e;
    if (getenv("STUB_BAD")) r.endSampleIndex = e + 1000000;
    if (logcb) logcb(2, "KWS", "classifier_score=0.950,active_threshold=0.923,detection_type=Accept");
    rescb("pw-dec", &r);
}

int PryonDecoder_PushAudioEventSamples(const char *id, uint64_t idx, const int16_t *s, uint32_t n) {
    (void)id;
    for (uint32_t i = 0; i < n; i++) {
        int a = abs(s[i]);
        int lv = a >= 20000 ? 3 : a >= 4000 ? 2 : a >= 1500 ? 1 : 0;
        if (lv == level) { if (lv) run++; continue; }
        if (level && run >= 400) {
            uint64_t end = idx + i;
            fire(level == 3 ? "STOP" : "ALEXA", level == 1 ? 0 : 2, run_start, end);
        }
        level = lv; run = lv ? 1 : 0; run_start = idx + i;
    }
    return 0;
}
