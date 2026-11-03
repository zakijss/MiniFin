/* Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include "video_hw.h"

#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <arm_neon.h>

#include "log.h"
#include "mi_vdec.h"

#define CHN 0
/* Output queue depth: the decoder allocates its output frames from the
 * 21 MB MMA heap (4 MB of it is the framebuffer), 0.46 MB each at 640x480
 * but 1.4 MB at 720p, where its reference frames take about 12 MB too. */
#define QUEUE_SMALL 5
#define QUEUE_BIG 3

struct Vdec {
    MI_SYS_ChnPort_t port;
    MI_VDEC_ChnAttr_t attr;
    int queue;
    int open;
    struct { uint64_t phys; void *vir; uint32_t len; } map[24]; /* decoder buffers we have mapped */
    int nmap;
};

static volatile int g_chn_open; /* for vdec_emergency_close */

Vdec *vdec_open(int hevc, int width, int height)
{
    Vdec *v = calloc(1, sizeof *v);
    MI_VDEC_InitParam_t ip;
    MI_VDEC_ChnAttr_t ca;
    MI_S32 r;
    if (!v) return NULL;
    memset(&ip, 0, sizeof ip);
    /* Low-latency mode (the default) hands out plain NV12 frames in decode
     * order; the player puts them in display order. With it off the
     * decoder hands out metadata buffers meant for MI_DIVP, and MI_DIVP
     * does not work on this firmware (its IRQ request fails). */
    ip.bDisableLowLatency = FALSE;
    r = MI_VDEC_InitDev(&ip);
    if (r != MI_SUCCESS) plog("vdec: InitDev 0x%x (continuing)", (unsigned)r);
    /* linear NV12 out, not the decoder's private tile format */
    MI_VDEC_SetOutputPortLayoutMode(E_MI_VDEC_OUTBUF_LAYOUT_LINEAR);
    memset(&ca, 0, sizeof ca);
    ca.eCodecType = hevc ? E_MI_VDEC_CODEC_TYPE_H265 : E_MI_VDEC_CODEC_TYPE_H264;
    ca.u32BufSize = 1024 * 1024;
    ca.u32Priority = 0;
    ca.u32PicWidth = (MI_U32)(width > 0 ? width : 640);
    ca.u32PicHeight = (MI_U32)(height > 0 ? height : 480);
    ca.eVideoMode = E_MI_VDEC_VIDEO_MODE_FRAME;
    ca.eDpbBufMode = E_MI_VDEC_DPB_MODE_NORMAL;
    int big = (long)ca.u32PicWidth * ca.u32PicHeight > 640L * 480;
    /* 5 references is plenty for MiniFin's transcodes (x264 veryfast uses
     * 1); above 640x480 there is only room for 3 */
    ca.stVdecVideoAttr.u32RefFrameNum = big ? 3 : 5;
    v->queue = big ? QUEUE_BIG : QUEUE_SMALL;
    r = MI_VDEC_CreateChn(CHN, &ca);
    if (r == MI_ERR_VDEC_CHN_EXIST) { /* left over by a killed player */
        plog("vdec: channel exists, recreating");
        MI_VDEC_StopChn(CHN);
        MI_VDEC_DestroyChn(CHN);
        r = MI_VDEC_CreateChn(CHN, &ca);
    }
    if (r != MI_SUCCESS) {
        plog("vdec: CreateChn 0x%x", (unsigned)r);
        MI_VDEC_DeInitDev();
        free(v);
        return NULL;
    }
    g_chn_open = 1;
    v->attr = ca;
    r = MI_VDEC_StartChn(CHN);
    if (r != MI_SUCCESS) {
        plog("vdec: StartChn 0x%x", (unsigned)r);
        MI_VDEC_DestroyChn(CHN);
        MI_VDEC_DeInitDev();
        g_chn_open = 0;
        free(v);
        return NULL;
    }
    v->port.eModId = E_MI_MODULE_ID_VDEC;
    v->port.u32DevId = 0;
    v->port.u32ChnId = CHN;
    v->port.u32PortId = 0;
    /* Frames are copied out at once, and the queue has room for every frame
     * in flight (MI_SYS overwrites the oldest when it is full). The decoder
     * allocates about this many linear buffers, so it is kept small: at
     * 720p each is 1.4 MB of a 21 MB MMA heap. */
    MI_SYS_SetChnOutputPortDepth(&v->port, 1, (MI_U32)v->queue);
    v->open = 1;
    plog("vdec: %s %dx%d open", hevc ? "H.265" : "H.264", width, height);
    return v;
}

int vdec_send(Vdec *v, const uint8_t *data, int len, uint64_t pts)
{
    MI_VDEC_VideoStream_t es;
    MI_S32 r;
    memset(&es, 0, sizeof es);
    es.pu8Addr = (MI_U8 *)data;
    es.u32Len = (MI_U32)len;
    es.u64PTS = pts;
    es.bEndOfFrame = TRUE;
    r = MI_VDEC_SendStream(CHN, &es, 0);
    if (r == MI_SUCCESS) return 0;
    if (r == MI_ERR_VDEC_BUF_FULL || r == MI_ERR_VDEC_NOBUF || r == MI_ERR_VDEC_BUSY) return 1;
    plog("vdec: SendStream 0x%x", (unsigned)r);
    return -1;
}

void vdec_send_eos(Vdec *v)
{
    MI_VDEC_VideoStream_t es;
    memset(&es, 0, sizeof es);
    es.bEndOfFrame = TRUE;
    es.bEndOfStream = TRUE;
    MI_VDEC_SendStream(CHN, &es, 20);
}

int vdec_get(Vdec *v, HwFrame *f)
{
    memset(f, 0, sizeof *f);
    if (MI_SYS_ChnOutputPortGetBuf(&v->port, &f->info, &f->h) != MI_SUCCESS) return 0;
    f->pts = f->info.u64Pts;
    f->valid = 1;
    return 1;
}

void vdec_put(Vdec *v, HwFrame *f)
{
    if (f->valid) MI_SYS_ChnOutputPortPutBuf(f->h);
    f->valid = 0;
}

/* Maps [phys, phys+len) (cached), from the page boundary below phys:
 * MI_SYS_Mmap wants page-aligned addresses and sizes, and a 640x368 frame's
 * chroma plane does not start on one. */
static void *map_buf(Vdec *v, uint64_t phys, uint32_t len)
{
    uint64_t base = phys & ~(uint64_t)4095;
    uint32_t off = (uint32_t)(phys - base), mlen = (off + len + 4095) & ~4095u;
    for (int i = 0; i < v->nmap; i++)
        if (v->map[i].phys == base && v->map[i].len >= mlen) return (uint8_t *)v->map[i].vir + off;
    void *vir = NULL;
    if (MI_SYS_Mmap(base, mlen, &vir, TRUE) != MI_SUCCESS) {
        plog("vdec: cannot map 0x%llx+%u", (unsigned long long)base, mlen);
        return NULL;
    }
    if (v->nmap == (int)(sizeof v->map / sizeof v->map[0])) { /* full: forget the oldest */
        MI_SYS_Munmap(v->map[0].vir, v->map[0].len);
        memmove(v->map, v->map + 1, sizeof v->map[0] * (size_t)--v->nmap);
    }
    v->map[v->nmap].phys = base;
    v->map[v->nmap].vir = vir;
    v->map[v->nmap].len = mlen;
    v->nmap++;
    return (uint8_t *)vir + off;
}

static void crop_of(const HwFrame *f, int *x, int *y, int *w, int *h)
{
    const MI_SYS_FrameData_t *fd = &f->info.stFrameData;
    *x = 0;
    *y = 0;
    *w = fd->u16Width;
    *h = fd->u16Height;
    if (fd->stContentCropWindow.u16Width && fd->stContentCropWindow.u16Height) {
        *x = fd->stContentCropWindow.u16X & ~1;
        *y = fd->stContentCropWindow.u16Y & ~1;
        *w = fd->stContentCropWindow.u16Width;
        *h = fd->stContentCropWindow.u16Height;
    }
}

static int factor(int w, int h)
{
    int k = (w + 639) / 640, kh = (h + 479) / 480;
    if (kh > k) k = kh;
    return k < 1 ? 1 : k;
}

void vdec_frame_size(const HwFrame *f, int *w, int *h)
{
    int x, y, cw, ch, k;
    crop_of(f, &x, &y, &cw, &ch);
    k = factor(cw, ch);
    *w = (cw / k) & ~1;
    *h = (ch / k) & ~1;
}

int vdec_convert(Vdec *v, HwFrame *f, uint8_t *dst, int dst_stride)
{
    MI_SYS_FrameData_t *fd = &f->info.stFrameData;
    int cx, cy, cw, ch, ow, oh, k;
    if (f->info.eBufType != E_MI_SYS_BUFDATA_FRAME || fd->ePixelFormat != E_MI_SYS_PIXEL_FRAME_YUV_SEMIPLANAR_420)
        return -1;
    crop_of(f, &cx, &cy, &cw, &ch);
    vdec_frame_size(f, &ow, &oh);
    k = factor(cw, ch);
    if (ow <= 0 || oh <= 0) return -1;
    uint32_t ys = fd->u32Stride[0], uvs = fd->u32Stride[1];
    uint32_t ylen = ys * fd->u16Height, uvlen = uvs * ((fd->u16Height + 1) / 2);
    uint8_t *Y = map_buf(v, fd->phyAddr[0], ylen), *UV = map_buf(v, fd->phyAddr[1], uvlen);
    if (!Y || !UV) return -1;
    /* the decoder wrote these with DMA: drop any stale cache lines (the
     * flush works on whole cache lines, so start on one) */
    MI_SYS_FlushInvCache((void *)((uintptr_t)Y & ~(uintptr_t)63), ylen + 64);
    MI_SYS_FlushInvCache((void *)((uintptr_t)UV & ~(uintptr_t)63), uvlen + 64);
    for (int j = 0; j < oh; j++) {
        int sy = cy + j * k;
        const uint8_t *yr = Y + (size_t)sy * ys + cx, *cr = UV + (size_t)(sy / 2) * uvs + cx;
        uint8_t *o = dst + (size_t)j * dst_stride;
        int i = 0;
        if (k == 1) {
            for (; i + 16 <= ow; i += 16) {
                uint8x8x2_t yy = vld2_u8(yr + i); /* even, odd luma */
                uint8x8x2_t uv = vld2_u8(cr + i); /* U, V */
                uint8x8x4_t out = { { yy.val[0], uv.val[1], yy.val[1], uv.val[0] } };
                vst4_u8(o + i * 2, out);
            }
            for (; i + 2 <= ow; i += 2) {
                o[i * 2] = yr[i];
                o[i * 2 + 1] = cr[i + 1];
                o[i * 2 + 2] = yr[i + 1];
                o[i * 2 + 3] = cr[i];
            }
        } else {
            for (; i + 2 <= ow; i += 2) {
                int sx = i * k;
                const uint8_t *c = cr + (sx & ~1);
                o[i * 2] = yr[sx];
                o[i * 2 + 1] = c[1];
                o[i * 2 + 2] = yr[sx + k];
                o[i * 2 + 3] = c[0];
            }
        }
    }
    return 0;
}

int vdec_alive(const Vdec *v) { return v && v->open; }

int vdec_queue(const Vdec *v) { return v->queue; }

int vdec_reset(Vdec *v)
{
    /* Stop/Reset/Start leaves the channel producing nothing on this
     * firmware, so a seek gets a fresh channel (a few milliseconds). */
    double t0 = now_sec();
    MI_VDEC_StopChn(CHN);
    MI_VDEC_DestroyChn(CHN);
    for (int i = 0; i < v->nmap; i++) MI_SYS_Munmap(v->map[i].vir, v->map[i].len);
    v->nmap = 0;
    g_chn_open = 0;
    MI_S32 r = MI_VDEC_CreateChn(CHN, &v->attr);
    if (r == MI_SUCCESS) {
        g_chn_open = 1;
        r = MI_VDEC_StartChn(CHN);
    }
    if (r != MI_SUCCESS) {
        plog("vdec: recreating the channel failed: 0x%x", (unsigned)r);
        if (g_chn_open) MI_VDEC_DestroyChn(CHN);
        g_chn_open = 0;
        MI_VDEC_DeInitDev();
        v->open = 0;
        return -1;
    }
    MI_SYS_SetChnOutputPortDepth(&v->port, 1, (MI_U32)v->queue);
    plog("vdec: channel reset in %.0f ms", (now_sec() - t0) * 1000);
    return 0;
}

void vdec_stats(Vdec *v, unsigned *received, unsigned *decoded, int *err)
{
    MI_VDEC_ChnStat_t s;
    memset(&s, 0, sizeof s);
    MI_VDEC_GetChnStat(CHN, &s);
    *received = s.u32RecvStreamFrames;
    *decoded = s.u32DecodeStreamFrames;
    *err = s.eErrCode;
}

void vdec_close(Vdec *v)
{
    if (!v) return;
    for (int i = 0; i < v->nmap; i++) MI_SYS_Munmap(v->map[i].vir, v->map[i].len);
    if (v->open) {
        MI_VDEC_StopChn(CHN);
        MI_VDEC_DestroyChn(CHN);
        MI_VDEC_DeInitDev();
        g_chn_open = 0;
    }
    free(v);
}

void vdec_emergency_close(void)
{
    if (!g_chn_open) return;
    g_chn_open = 0;
    MI_VDEC_StopChn(CHN);
    MI_VDEC_DestroyChn(CHN);
    MI_VDEC_DeInitDev();
}
