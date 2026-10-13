import { get } from './api.js';
import { createStore } from './store.js';

// Where the library's files are (#95, #99, #100): Google Drive, and the storage locations besides
// it. What deleting and missing files say depends on it: with Drive alone, the words Drive users
// know.
export const places = createStore({ loaded: false, stores: [], default: 'drive' });

export function loadPlaces() {
  return get('/storage').then((v) => {
    places.set({ loaded: true, stores: v.stores, default: v.default });
    return v;
  }, () => null);
}

// driveOnly says whether the library is in Google Drive alone.
export const driveOnly = () => !places.get().stores.length;

// trashNote says where deleted songs' files go.
export function trashNote() {
  return driveOnly()
    ? '沒有其他歌曲使用的音檔會移到 Google Drive 垃圾桶（30 天內可在 Drive 還原）'
    : '沒有其他歌曲使用的音檔會移到它所在位置的垃圾桶（Google Drive、OneDrive 的垃圾桶，或資料夾和 R2 裡的 .kanade-trash，30 天內可還原）；只讀來源裡的檔案留在原處';
}

// trashName is the trash a file that could not be moved there was meant for.
export const trashName = () => (driveOnly() ? 'Drive 垃圾桶' : '垃圾桶');

// missingNote says what became of a song's file that is gone, by where it was (place: from the
// server; none for Drive's older answers).
export function missingNote(place) {
  if (!place || place.kind === 'drive') {
    return '這首歌的音檔已從 Google Drive 遺失，暫時無法播放。從 Drive 垃圾桶還原後會自動恢復；不要了的話可以永久刪除。';
  }
  if (place.mode === 'source') {
    return `這首歌的音檔已不在來源「${place.name}」（被刪除、搬走或內容改變），暫時無法播放。檔案回來後，下次掃描會自動恢復；不要了的話可以永久刪除。`;
  }
  return `這首歌的音檔已不在「${place.name}」，暫時無法播放。檔案放回原位後，下次檢查（或設定頁「儲存位置」的「檢查檔案」）會自動恢復；不要了的話可以永久刪除。`;
}
