/* Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include "display.h"

#include <fcntl.h>
#include <linux/fb.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/syscall.h>
#include <unistd.h>

#include "mi_gfx.h"
#include "mi_sys.h"
#include "log.h"

struct Display {
    int fd;
    struct fb_var_screeninfo v;
    struct fb_fix_screeninfo f;
    uint8_t *mem;      /* CPU mapping of the whole framebuffer */
    size_t memlen;
    int W, H, line;
    int back;          /* buffer to draw next (0 or 1) */
    int hw;            /* MI_SYS + MI_GFX in use */

    int vx, vy, vw, vh; /* video rectangle, upright screen coordinates */
    /* hw: YUV 4:2:2 source */
    uint64_t src_phys;
    int src_stride, sx, sy, sw, sh;
    uint8_t *yuv;
    uint64_t yuv_phys;
    size_t yuv_cap;
    int yuv_stride, yuv_w, yuv_h;
    uint32_t *stage;
    uint64_t stage_phys;
    size_t stage_len;
    int stage_cap;      /* bytes allocated */

    Canvas osd;
    uint64_t osd_phys;
    OsdRect rects[OSD_MAX_RECTS];
    int nrects;
};

static Display *g_disp; /* for display_emergency_blank */

static int mma_alloc(size_t len, uint64_t *phys, void **vir)
{
    MI_PHY p = 0;
    void *v = NULL;
    if (MI_SYS_MMA_Alloc(NULL, (MI_U32)len, &p) != MI_SUCCESS) return -1;
    if (MI_SYS_Mmap(p, (MI_U32)len, &v, TRUE) != MI_SUCCESS) {
        MI_SYS_MMA_Free(p);
        return -1;
    }
    *phys = p;
    *vir = v;
    return 0;
}

static void mma_free(uint64_t phys, void *vir, size_t len)
{
    if (vir) MI_SYS_Munmap(vir, (MI_U32)len);
    if (phys) MI_SYS_MMA_Free(phys);
}

Display *display_open(int hw)
{
    Display *d = calloc(1, sizeof *d);
    if (!d) return NULL;
    /* Raw syscall: if Onion's libpadsp.so is preloaded it wraps open() and
     * has its own idea about /dev/fb0. */
    d->fd = (int)syscall(SYS_openat, AT_FDCWD, "/dev/fb0", O_RDWR | O_CLOEXEC);
    if (d->fd < 0 || ioctl(d->fd, FBIOGET_FSCREENINFO, &d->f) < 0 || ioctl(d->fd, FBIOGET_VSCREENINFO, &d->v) < 0) {
        plog("display: cannot open /dev/fb0");
        if (d->fd >= 0) close(d->fd);
        free(d);
        return NULL;
    }
    d->W = (int)d->v.xres;
    d->H = (int)d->v.yres;
    d->line = (int)d->f.line_length;
    if (d->v.bits_per_pixel != 32 || d->W <= 0 || d->H <= 0) {
        plog("display: unsupported framebuffer %ux%u %u bpp", d->v.xres, d->v.yres, d->v.bits_per_pixel);
        close(d->fd);
        free(d);
        return NULL;
    }
    if (d->v.yres_virtual < d->v.yres * 2) { /* make sure there are two buffers */
        d->v.yres_virtual = d->v.yres * 2;
        d->v.yoffset = 0;
        ioctl(d->fd, FBIOPUT_VSCREENINFO, &d->v);
        ioctl(d->fd, FBIOGET_VSCREENINFO, &d->v);
        ioctl(d->fd, FBIOGET_FSCREENINFO, &d->f);
    }
    d->memlen = (size_t)d->line * d->H * 2;
    if (d->memlen > d->f.smem_len) d->memlen = d->f.smem_len;
    d->mem = mmap(NULL, d->memlen, PROT_READ | PROT_WRITE, MAP_SHARED, d->fd, 0);
    if (d->mem == MAP_FAILED) d->mem = NULL;
    d->back = d->v.yoffset >= d->v.yres ? 0 : 1;

    d->osd.w = d->W;
    d->osd.h = d->H;
    d->osd.stride = d->W;
    if (hw) {
        void *v = NULL;
        if (MI_SYS_Init() == MI_SUCCESS && MI_GFX_Open() == MI_SUCCESS &&
            mma_alloc((size_t)d->W * d->H * 4, &d->osd_phys, &v) == 0) {
            d->osd.px = v;
            d->hw = 1;
        } else {
            plog("display: MI_GFX unavailable, drawing with the CPU");
        }
    }
    if (!d->hw) {
        d->osd.px = calloc((size_t)d->W * d->H, 4);
        if (!d->osd.px || !d->mem) {
            plog("display: out of memory");
            free(d->osd.px);
            if (d->mem) munmap(d->mem, d->memlen);
            close(d->fd);
            free(d);
            return NULL;
        }
    }
    memset(d->osd.px, 0, (size_t)d->W * d->H * 4);
    if (d->hw) MI_SYS_FlushInvCache(d->osd.px, (MI_U32)(d->W * d->H * 4));
    d->vw = d->W;
    d->vh = d->H;
    g_disp = d;
    plog("display: %dx%d line %d, %s", d->W, d->H, d->line, d->hw ? "MI_GFX" : "CPU");
    return d;
}

int display_hw(const Display *d) { return d->hw; }

int display_set_video(Display *d, int src_w, int src_h, double sar)
{
    double dar;
    int w, h;
    if (src_w <= 0 || src_h <= 0) return -1;
    if (!(sar > 0.1 && sar < 10)) sar = 1;
    dar = src_w * sar / src_h;
    if (dar * d->H >= d->W) { /* wider than the screen: bars above and below */
        w = d->W;
        h = (int)(d->W / dar + 0.5);
    } else {
        h = d->H;
        w = (int)(d->H * dar + 0.5);
    }
    w = (w + 8) & ~15; /* the scalers want aligned widths */
    if (w > d->W) w = d->W;
    if (w < 16) w = 16;
    h &= ~1;
    if (h > d->H) h = d->H;
    if (h < 2) h = 2;
    d->vw = w;
    d->vh = h;
    d->vx = ((d->W - w) / 2) & ~1;
    d->vy = ((d->H - h) / 2) & ~1;

    size_t need = (size_t)w * h * 4;
    if (!d->hw && (size_t)d->stage_cap < need) {
        free(d->stage);
        d->stage = calloc((size_t)d->W * d->H, 4);
        if (!d->stage) return -1;
        d->stage_cap = d->W * d->H * 4;
    }
    d->stage_len = need;
    plog("display: video %dx%d sar %.3f -> %dx%d at %d,%d", src_w, src_h, sar, w, h, d->vx, d->vy);
    return 0;
}

void display_video_rect(const Display *d, int *x, int *y, int *w, int *h)
{
    *x = d->vx;
    *y = d->vy;
    *w = d->vw;
    *h = d->vh;
}

uint32_t *display_stage(Display *d, uint64_t *phys, int *stride_px)
{
    if (phys) *phys = d->stage_phys;
    if (stride_px) *stride_px = d->vw;
    return d->stage;
}

void display_stage_written(Display *d)
{
    if (d->hw && d->yuv) MI_SYS_FlushInvCache(d->yuv, (MI_U32)(d->yuv_stride * d->yuv_h));
}

void display_set_source_yuv(Display *d, uint64_t phys, int stride_bytes, int x, int y, int w, int h)
{
    d->src_phys = phys;
    d->src_stride = stride_bytes;
    d->sx = x;
    d->sy = y;
    d->sw = w;
    d->sh = h;
}

uint8_t *display_yuv_buffer(Display *d, int w, int h, int *stride_bytes)
{
    int stride = ((w + 15) & ~15) * 2;
    size_t need = (size_t)stride * (size_t)h;
    if (!d->hw) return NULL;
    if (need > d->yuv_cap) {
        uint64_t p = 0;
        void *v = NULL;
        mma_free(d->yuv_phys, d->yuv, d->yuv_cap);
        d->yuv = NULL;
        d->yuv_phys = 0;
        d->yuv_cap = 0;
        if (mma_alloc(need, &p, &v) < 0) return NULL;
        d->yuv = v;
        d->yuv_phys = p;
        d->yuv_cap = need;
    }
    d->yuv_stride = stride;
    d->yuv_w = w;
    d->yuv_h = h;
    display_set_source_yuv(d, d->yuv_phys, stride, 0, 0, w, h);
    *stride_bytes = stride;
    return d->yuv;
}

void *display_mma_alloc(size_t len, uint64_t *phys)
{
    void *v = NULL;
    if (mma_alloc(len, phys, &v) < 0) return NULL;
    return v;
}

void display_mma_free(void *vir, uint64_t phys, size_t len) { mma_free(phys, vir, len); }

Canvas *display_osd(Display *d) { return &d->osd; }

void display_osd_changed(Display *d, const OsdRect *rects, int n)
{
    if (n > OSD_MAX_RECTS) n = OSD_MAX_RECTS;
    memcpy(d->rects, rects, sizeof *rects * (size_t)n);
    d->nrects = n;
    if (d->hw) MI_SYS_FlushInvCache(d->osd.px, (MI_U32)(d->W * d->H * 4));
}

/* ---- MI_GFX path ---- */

static MI_GFX_Surface_t fb_surface(const Display *d, int buf)
{
    MI_GFX_Surface_t s;
    s.phyAddr = d->f.smem_start + (MI_PHY)buf * d->H * d->line;
    s.eColorFmt = E_MI_GFX_FMT_ARGB8888;
    s.u32Width = (MI_U32)d->W;
    s.u32Height = (MI_U32)d->H;
    s.u32Stride = (MI_U32)d->line;
    return s;
}

/* An upright rectangle's place on the upside-down panel. */
static MI_GFX_Rect_t flipped(const Display *d, int x, int y, int w, int h)
{
    MI_GFX_Rect_t r;
    r.s32Xpos = d->W - x - w;
    r.s32Ypos = d->H - y - h;
    r.u32Width = (MI_U32)w;
    r.u32Height = (MI_U32)h;
    return r;
}

static void gfx_check(const char *what, MI_S32 r);
static void gfx_fill(MI_GFX_Surface_t *dst, MI_GFX_Rect_t r, MI_U16 *fence)
{
    if (r.u32Width && r.u32Height) gfx_check("fill", MI_GFX_QuickFill(dst, &r, 0xff000000, fence));
}

static int gfx_errors;
static int blend_mode = -1;

static void gfx_check(const char *what, MI_S32 r)
{
    if (r != MI_SUCCESS && gfx_errors++ < 8) plog("display: %s -> 0x%x", what, (unsigned)r);
}

static void present_gfx(Display *d)
{
    if (blend_mode < 0) {
        const char *e = getenv("MINIFIN_BLEND");
        blend_mode = e ? atoi(e) : 0;
    }
    MI_GFX_Surface_t dst = fb_surface(d, d->back), src;
    MI_GFX_Rect_t sr, dr;
    MI_GFX_Opt_t opt;
    MI_U16 fence = 0;

    /* bars around the picture */
    if (d->vy > 0) {
        gfx_fill(&dst, flipped(d, 0, 0, d->W, d->vy), &fence);
        gfx_fill(&dst, flipped(d, 0, d->vy + d->vh, d->W, d->H - d->vy - d->vh), &fence);
    }
    if (d->vx > 0) {
        gfx_fill(&dst, flipped(d, 0, d->vy, d->vx, d->vh), &fence);
        gfx_fill(&dst, flipped(d, d->vx + d->vw, d->vy, d->W - d->vx - d->vw, d->vh), &fence);
    }
    if (d->src_phys && d->sw > 0 && d->sh > 0) {
        memset(&opt, 0, sizeof opt);
        src.phyAddr = d->src_phys;
        src.eColorFmt = E_MI_GFX_FMT_YUV422;
        src.u32Width = (MI_U32)(d->sx + d->sw);
        src.u32Height = (MI_U32)(d->sy + d->sh);
        src.u32Stride = (MI_U32)d->src_stride;
        sr.s32Xpos = d->sx;
        sr.s32Ypos = d->sy;
        sr.u32Width = (MI_U32)d->sw;
        sr.u32Height = (MI_U32)d->sh;
        dr = flipped(d, d->vx, d->vy, d->vw, d->vh);
        opt.eRotate = E_MI_GFX_ROTATE_180;
        opt.eSrcDfbBldOp = E_MI_GFX_DFB_BLD_ONE;
        opt.eDstDfbBldOp = E_MI_GFX_DFB_BLD_ZERO;
        gfx_check("video blit", MI_GFX_BitBlit(&src, &sr, &dst, &dr, &opt, &fence));
    } else {
        gfx_fill(&dst, flipped(d, d->vx, d->vy, d->vw, d->vh), &fence);
    }
    /* overlay, alpha blended */
    for (int i = 0; i < d->nrects; i++) {
        const OsdRect *r = &d->rects[i];
        memset(&opt, 0, sizeof opt);
        src.phyAddr = d->osd_phys;
        src.eColorFmt = E_MI_GFX_FMT_ARGB8888;
        src.u32Width = (MI_U32)d->W;
        src.u32Height = (MI_U32)d->H;
        src.u32Stride = (MI_U32)d->W * 4;
        sr.s32Xpos = r->x;
        sr.s32Ypos = r->y;
        sr.u32Width = (MI_U32)r->w;
        sr.u32Height = (MI_U32)r->h;
        dr = flipped(d, r->x, r->y, r->w, r->h);
        opt.eRotate = E_MI_GFX_ROTATE_180;
        switch (blend_mode) {
        case 1: /* plain copy: tells blending apart from blitting problems */
            opt.eSrcDfbBldOp = E_MI_GFX_DFB_BLD_ONE;
            opt.eDstDfbBldOp = E_MI_GFX_DFB_BLD_ZERO;
            break;
        case 2:
            opt.eDFBBlendFlag = E_MI_GFX_DFB_BLEND_ALPHACHANNEL | E_MI_GFX_DFB_BLEND_COLORALPHA;
            opt.u32GlobalSrcConstColor = 0xff000000;
            opt.eSrcDfbBldOp = E_MI_GFX_DFB_BLD_SRCALPHA;
            opt.eDstDfbBldOp = E_MI_GFX_DFB_BLD_INVSRCALPHA;
            break;
        case 3:
            opt.eSrcDfbBldOp = E_MI_GFX_DFB_BLD_SRCALPHA;
            opt.eDstDfbBldOp = E_MI_GFX_DFB_BLD_INVSRCALPHA;
            break;
        default:
            opt.eDFBBlendFlag = E_MI_GFX_DFB_BLEND_ALPHACHANNEL;
            opt.eSrcDfbBldOp = E_MI_GFX_DFB_BLD_SRCALPHA;
            opt.eDstDfbBldOp = E_MI_GFX_DFB_BLD_INVSRCALPHA;
        }
        gfx_check("overlay blit", MI_GFX_BitBlit(&src, &sr, &dst, &dr, &opt, &fence));
    }
    MI_GFX_WaitAllDone(TRUE, fence);
}

/* ---- CPU path ---- */

static inline uint32_t blend(uint32_t under, uint32_t over)
{
    uint32_t a = over >> 24, ia = 255 - a;
    uint32_t rb = ((over & 0xff00ff) * a + (under & 0xff00ff) * ia) >> 8 & 0xff00ff;
    uint32_t g = ((over & 0x00ff00) * a + (under & 0x00ff00) * ia) >> 8 & 0x00ff00;
    return 0xff000000 | rb | g;
}

static void present_cpu(Display *d)
{
    uint8_t *base = d->mem + (size_t)d->back * d->H * d->line;
    uint32_t line[1024];
    int W = d->W > 1024 ? 1024 : d->W;
    for (int y = 0; y < d->H; y++) {
        int in_video = d->stage && y >= d->vy && y < d->vy + d->vh;
        for (int x = 0; x < W; x++) line[x] = 0xff000000;
        if (in_video) {
            const uint32_t *s = d->stage + (size_t)(y - d->vy) * d->vw;
            memcpy(line + d->vx, s, (size_t)d->vw * 4);
        }
        for (int i = 0; i < d->nrects; i++) {
            const OsdRect *r = &d->rects[i];
            if (y < r->y || y >= r->y + r->h) continue;
            const uint32_t *o = d->osd.px + (size_t)y * d->osd.stride;
            for (int x = r->x; x < r->x + r->w && x < W; x++)
                if (o[x] >> 24) line[x] = blend(line[x], o[x]);
        }
        /* write the row upside down */
        uint32_t *dst = (uint32_t *)(base + (size_t)(d->H - 1 - y) * d->line);
        for (int x = 0; x < W; x++) dst[W - 1 - x] = line[x];
    }
}

void display_present(Display *d)
{
    if (d->hw) present_gfx(d);
    else present_cpu(d);
    d->v.yoffset = (unsigned)(d->back * d->H);
    ioctl(d->fd, FBIOPAN_DISPLAY, &d->v);
    d->back ^= 1;
}

void display_blank(Display *d)
{
    if (d->mem) memset(d->mem, 0, d->memlen);
    else if (d->hw) MI_SYS_MemsetPa(d->f.smem_start, 0, (MI_U32)(d->line * d->H * 2));
    d->v.yoffset = 0;
    ioctl(d->fd, FBIOPAN_DISPLAY, &d->v);
    d->back = 1;
}

void display_emergency_blank(void)
{
    Display *d = g_disp;
    if (!d) return;
    if (d->mem) memset(d->mem, 0, d->memlen);
    d->v.yoffset = 0;
    ioctl(d->fd, FBIOPAN_DISPLAY, &d->v);
}

void display_close(Display *d)
{
    if (!d) return;
    display_blank(d);
    g_disp = NULL;
    if (d->hw) {
        mma_free(d->yuv_phys, d->yuv, d->yuv_cap);
        mma_free(d->osd_phys, d->osd.px, (size_t)d->W * d->H * 4);
        MI_GFX_Close();
        /* MI_SYS_Exit is left to the caller (VDEC shares MI_SYS). */
    } else {
        free(d->stage);
        free(d->osd.px);
    }
    if (d->mem) munmap(d->mem, d->memlen);
    close(d->fd);
    free(d);
}
